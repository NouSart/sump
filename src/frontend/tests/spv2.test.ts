import assert from 'node:assert/strict';
import test from 'node:test';
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import { WebSocketServer } from 'ws';
import { decode, encode } from '@msgpack/msgpack';
import {
  SPv2RemoteError,
  SPv2Session,
  SPv2UnavailableError,
  checkSPv2Trust,
  createSPv2WSS,
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

test('createSPv2WSS 处理 deliver 并回执', async () => {
  let delivered: { conversationId: string; text: string } | null = null;
  const wss = createSPv2WSS(
    'frontend',
    () => {},
    {
      onDeliver: (m) => {
        delivered = { conversationId: m.conversationId, text: m.text };
      },
      onTimers: () => true,
      onApproval: () => {},
      onProgress: () => true,
      onTerminal: () => true,
    },
  );
  const server = createServer();
  server.on('upgrade', (req, socket, head) => {
    wss.handleUpgrade(req, socket, head, (ws) => wss.emit('connection', ws, req));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()));
  const port = (server.address() as AddressInfo).port;
  try {
    const out = await dialSPv2Once(
      `ws://127.0.0.1:${port}`,
      'reasoner',
      'frontend',
      'deliver',
      { text: 'hi', conversation_id: 'web:private:owner' },
      2000,
    );
    assert.deepEqual(out, { delivered: true });
    assert.deepEqual(delivered, { conversationId: 'web:private:owner', text: 'hi' });
  } finally {
    for (const ws of wss.clients) ws.terminate();
    await new Promise<void>((resolve) => wss.close(() => resolve()));
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
});

test('spv2Addr 处理末尾斜杠，不产生双斜杠', () => {
  assert.equal(spv2Addr('ws://h:1/'), 'ws://h:1/spv2');
});

test('dialSPv2Once 忽略 reply_to 不匹配的 reply', async () => {
  const wss = new WebSocketServer({ port: 0 });
  const port = await listen(wss);
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      const f = decode(data as Uint8Array) as Record<string, unknown>;
      ws.send(
        encode({
          v: 'spv2.1',
          id: 'wrong',
          type: 'reply',
          work: f.work,
          from: { kind: 'service', id: 'peer' },
          to: f.from,
          reply_to: 'other',
          status: 'ok',
          body: encode({ text: 'wrong' }),
        }),
      );
    });
  });
  try {
    await assert.rejects(
      dialSPv2Once(`ws://127.0.0.1:${port}`, 'frontend', 'peer', 'echo', undefined, 300),
      /超时/,
    );
  } finally {
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }
});

test('dialSPv2Once 空错误码回落 internal', async () => {
  const wss = new WebSocketServer({ port: 0 });
  const port = await listen(wss);
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      const f = decode(data as Uint8Array) as Record<string, unknown>;
      ws.send(
        encode({
          v: 'spv2.1', id: 'e', type: 'reply', work: f.work,
          from: { kind: 'service', id: 'peer' }, to: f.from, reply_to: f.id,
          status: 'error', error: { code: '', message: 'bad', retryable: false },
        }),
      );
    });
  });
  try {
    await assert.rejects(
      dialSPv2Once(`ws://127.0.0.1:${port}`, 'frontend', 'peer', 'echo', undefined, 1000),
      (err: unknown) => err instanceof SPv2RemoteError && err.code === 'internal',
    );
  } finally {
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }
});

test('dialSPv2Once 空 body 解析为 undefined', async () => {
  const wss = new WebSocketServer({ port: 0 });
  const port = await listen(wss);
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      const f = decode(data as Uint8Array) as Record<string, unknown>;
      ws.send(
        encode({
          v: 'spv2.1', id: 'b', type: 'reply', work: f.work,
          from: { kind: 'service', id: 'peer' }, to: f.from, reply_to: f.id, status: 'ok', body: new Uint8Array(),
        }),
      );
    });
  });
  try {
    assert.equal(await dialSPv2Once(`ws://127.0.0.1:${port}`, 'frontend', 'peer', 'echo', undefined, 1000), undefined);
  } finally {
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }
});

async function withPortalEndpoint(
  handlers: Parameters<typeof createSPv2WSS>[2],
  fn: (url: string) => Promise<void>,
): Promise<void> {
  const wss = createSPv2WSS('frontend', () => {}, handlers);
  const server = createServer();
  server.on('upgrade', (req, socket, head) => {
    wss.handleUpgrade(req, socket, head, (ws) => wss.emit('connection', ws, req));
  });
  await new Promise<void>((resolve) => server.listen(0, '127.0.0.1', () => resolve()));
  const port = (server.address() as AddressInfo).port;
  try {
    await fn(`ws://127.0.0.1:${port}`);
  } finally {
    for (const ws of wss.clients) ws.terminate();
    await new Promise<void>((resolve) => wss.close(() => resolve()));
    await new Promise<void>((resolve) => server.close(() => resolve()));
  }
}

test('createSPv2WSS timer_state 与非法 progress/report_result', async () => {
  let timers = 0;
  let progress = 0;
  let terminal = 0;
  await withPortalEndpoint(
    {
      onDeliver: () => {},
      onTimers: () => { timers += 1; return true; },
      onApproval: () => {},
      onProgress: () => { progress += 1; return true; },
      onTerminal: () => { terminal += 1; return true; },
    },
    async (url) => {
      const ok = await dialSPv2Once(url, 'timer', 'frontend', 'timer_state', { boss: '', seq: 1, timers: [] }, 1000) as { accepted?: boolean };
      assert.equal(ok.accepted, true);
      const badProgress = await dialSPv2Once(url, 'reasoner', 'frontend', 'progress', { seq: 'bad' }, 1000) as { accepted?: boolean };
      assert.equal(badProgress.accepted, false);
      const badTerminal = await dialSPv2Once(url, 'shell', 'frontend', 'report_result', { item_id: '' }, 1000) as { accepted?: boolean };
      assert.equal(badTerminal.accepted, false);
    },
  );
  assert.equal(timers, 1);
  assert.equal(progress, 0);
  assert.equal(terminal, 0);
});

test('createSPv2WSS 未知 method 回 not_found', async () => {
  await withPortalEndpoint(
    { onDeliver: () => {}, onTimers: () => true, onApproval: () => {}, onProgress: () => true, onTerminal: () => true },
    async (url) => {
      await assert.rejects(
        dialSPv2Once(url, 'x', 'frontend', 'nope', undefined, 1000),
        (err: unknown) => err instanceof SPv2RemoteError && err.code === 'not_found',
      );
    },
  );
});

test('checkSPv2Trust 拒绝不受信 Host', () => {
  const cfg = { boundAddr: '127.0.0.1:1', trustedHosts: [] };
  assert.equal(checkSPv2Trust('127.0.0.1:1', undefined, cfg), null);
  assert.ok(checkSPv2Trust('evil.example', undefined, cfg));
});

test('SPv2Session 空 body/事件/错误/关闭边界', async () => {
  const wss = new WebSocketServer({ port: 0 });
  const port = await listen(wss);
  wss.on('connection', (ws) => {
    ws.on('message', (data) => {
      const f = decodeFrame(new Uint8Array(data as Buffer));
      if (f.type === 'call') {
        if (f.method === 'error') {
          ws.send(encodeFrame({
            v: 'spv2.1', id: 'e', type: 'reply', work: f.work,
            from: { kind: 'service', id: 'center' }, to: f.from, reply_to: f.id,
            status: 'error', error: { code: 'invalid_argument', message: 'bad', retryable: false },
          }));
          return;
        }
        ws.send(encodeFrame({
          v: 'spv2.1', id: 'r', type: 'reply', work: f.work,
          from: { kind: 'service', id: 'center' }, to: f.from, reply_to: f.id,
          status: 'ok', body: new Uint8Array(),
        }));
        return;
      }
      if (f.type === 'event') {
        ws.send(encodeFrame({
          v: 'spv2.1', id: 'ev', type: 'event', work: f.work,
          from: { kind: 'service', id: 'center' }, to: f.from,
          method: 'tick', body: f.method === 'withbody' ? encode({ x: 1 }) : undefined,
        }));
      }
    });
  });
  const events: { method: string; body: unknown }[] = [];
  try {
    const session = await SPv2Session.dial(`ws://127.0.0.1:${port}`, 'frontend', 'center', {
      onEvent: (ev) => events.push(ev),
    });
    assert.equal(await session.call('empty', undefined), undefined);
    await assert.rejects(session.call('error', undefined), (err: unknown) => err instanceof SPv2RemoteError);
    assert.equal(session.sendEvent('nobody', undefined), true);
    assert.equal(session.sendEvent('withbody', { x: 1 }), true);
    const deadline = Date.now() + 2000;
    while (events.length < 2 && Date.now() < deadline) await new Promise((r) => setTimeout(r, 5));
    assert.deepEqual(events, [
      { method: 'tick', body: undefined },
      { method: 'tick', body: { x: 1 } },
    ]);
    session.close();
    await assert.rejects(session.call('after-close', undefined), /SPv2 连接不可用/);
  } finally {
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }
});
