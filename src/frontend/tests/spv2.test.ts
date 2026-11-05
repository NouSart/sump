import assert from 'node:assert/strict';
import test from 'node:test';
import { WebSocketServer } from 'ws';
import { decode, encode } from '@msgpack/msgpack';
import {
  SPv2RemoteError,
  SPv2UnavailableError,
  dialSPv2Once,
  encodeFrame,
  decodeFrame,
  spv2Addr,
} from '../src/server/spv2.ts';

function listen(wss: WebSocketServer): Promise<number> {
  return new Promise((resolve) => {
    wss.on('listening', () => {
      const a = wss.address();
      resolve(typeof a === 'object' && a !== null ? a.port : 0);
    });
  });
}

test('spv2Addr 把 /ws 换成 /spv2', () => {
  assert.equal(spv2Addr('ws://h:1/ws'), 'ws://h:1/spv2');
  assert.equal(spv2Addr('ws://h:1'), 'ws://h:1/spv2');
  assert.equal(spv2Addr('ws://h:1/spv2'), 'ws://h:1/spv2');
});

test('encode/decode frame 保留 body 为 bin', () => {
  const f = {
    v: 'spv2.1',
    id: '01J0000000000000000000000',
    type: 'call',
    work: '01J0000000000000000000001',
    from: { kind: 'service', id: 'frontend' },
    to: { kind: 'service', id: 'reasoner' },
    method: 'message',
    content_type: 'application/msgpack',
    body: encode({ text: 'hi' }),
  };
  const back = decodeFrame(encodeFrame(f));
  assert.equal(back.type, 'call');
  assert.equal(back.from.id, 'frontend');
  assert.equal(back.to.id, 'reasoner');
  assert.ok(back.body instanceof Uint8Array);
  assert.deepEqual(decode(back.body!), { text: 'hi' });
});

test('dialSPv2Once 收发 call/reply', async () => {
  const wss = new WebSocketServer({ port: 0 });
  const port = await listen(wss);
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      const f = decode(data as Uint8Array) as Record<string, unknown>;
      assert.equal(f.type, 'call');
      assert.equal(f.method, 'echo');
      const reply = {
        v: 'spv2.1',
        id: '01J0000000000000000000002',
        type: 'reply',
        work: f.work,
        from: { kind: 'service', id: 'peer' },
        to: f.from,
        reply_to: f.id,
        status: 'ok',
        body: encode({ text: 'pong' }),
      };
      ws.send(encode(reply));
    });
  });
  try {
    const out = await dialSPv2Once(`ws://127.0.0.1:${port}`, 'frontend', 'peer', 'echo', { text: 'hi' }, 2000);
    assert.deepEqual(out, { text: 'pong' });
  } finally {
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }
});

test('dialSPv2Once 把 status=error 映射成 SPv2RemoteError', async () => {
  const wss = new WebSocketServer({ port: 0 });
  const port = await listen(wss);
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      const f = decode(data as Uint8Array) as Record<string, unknown>;
      ws.send(
        encode({
          v: 'spv2.1',
          id: '01J0000000000000000000003',
          type: 'reply',
          work: f.work,
          from: { kind: 'service', id: 'peer' },
          to: f.from,
          reply_to: f.id,
          status: 'error',
          error: { code: 'permission_denied', message: 'no', retryable: false },
        }),
      );
    });
  });
  try {
    await assert.rejects(
      dialSPv2Once(`ws://127.0.0.1:${port}`, 'frontend', 'peer', 'echo', undefined, 2000),
      (err: unknown) => err instanceof SPv2RemoteError && err.code === 'permission_denied',
    );
  } finally {
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }
});

test('dialSPv2Once 连接失败抛 SPv2UnavailableError', async () => {
  await assert.rejects(
    dialSPv2Once('ws://127.0.0.1:1', 'frontend', 'peer', 'echo', undefined, 500),
    (err: unknown) => err instanceof SPv2UnavailableError,
  );
});
