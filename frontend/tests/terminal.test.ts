// 终端通道：用户敲的命令经门户交给 shell，结果由 shell 的 report_result 补报回来。
//
// 这里守的是链路本身：受理回执有没有 item_id、补报能不能被认领、SSE 能不能把结果送到浏览器、
// 以及"跑过的命令要进会话记录"（终端与 agent 共享同一个工作现场）。
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { request as httpRequest } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { decode, encode } from '@msgpack/msgpack';
import { WebSocket, WebSocketServer, type RawData } from 'ws';
import { startFrontend, type FrontendHandle } from '../src/server/app.ts';
import { StubCenter } from './stub-center.ts';
import { waitFor } from './util.ts';

const silent = (): void => {};

interface Jump {
  to: string;
  action: string;
  input: unknown;
  from: string;
  boss: string;
}

async function makeWebDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'sump-web-'));
  await writeFile(join(dir, 'index.html'), '<!doctype html><title>面板</title><p>ok</p>', 'utf8');
  return dir;
}

// startStub：冒充 shell / memory（同一个 WS 端点，按 to 分辨），记录收到的 jump。
async function startStub(reply: { ok: boolean; data?: unknown; error?: string }): Promise<{
  url: string;
  jumps: Jump[];
  stop: () => Promise<void>;
}> {
  const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
  const jumps: Jump[] = [];
  wss.on('connection', (ws) => {
    ws.on('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      const env = decode(bytes) as {
        id: string;
        from: string;
        to: string;
        boss?: string;
        payload?: { action?: string; input?: unknown };
      };
      if (env.payload === undefined) return;
      jumps.push({
        to: env.to,
        action: env.payload.action ?? '',
        input: env.payload.input,
        from: env.from,
        boss: env.boss ?? '',
      });
      ws.send(encode({ v: 1, id: env.id, type: 'response', from: env.to, to: env.from, payload: reply }));
    });
  });
  await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
  const port = (wss.address() as { port: number }).port;
  const url = `ws://127.0.0.1:${String(port)}/ws`;
  return {
    url,
    jumps,
    stop: () =>
      new Promise<void>((resolve) => {
        for (const c of wss.clients) c.terminate();
        wss.close(() => resolve());
      }),
  };
}

async function startPortal(roster: { name: string; addr: string }[]): Promise<{ handle: FrontendHandle; stub: StubCenter }> {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = { services: [{ name: 'frontend', addr: 'ws://127.0.0.1:9/ws' }, ...roster], revision: 7 };
  const handle = await startFrontend(
    {
      name: 'frontend',
      addr: '127.0.0.1:0',
      centerURL,
      webDir: await makeWebDir(),
      heartbeatMS: 50,
      reasoner: 'reasoner',
      memory: 'memory',
      approval: 'approval',
      localesDir: await makeWebDir(),
      lang: 'zh-CN',
      shell: 'shell',
      trustedHosts: [],
    },
    silent,
  );
  await waitFor(() => handle.link.connected, 3000, '门户接入中心');
  return { handle, stub };
}

function postJSON(url: string, body: unknown): Promise<Response> {
  return fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// openSSE 打开事件流，回调里收事件名与数据。
function openSSE(addr: string, onEvent: (name: string, data: string) => void): Promise<{ close: () => void }> {
  const idx = addr.lastIndexOf(':');
  return new Promise((resolve, reject) => {
    const req = httpRequest(
      { host: addr.slice(0, idx), port: Number(addr.slice(idx + 1)), path: '/api/events', method: 'GET', headers: { host: addr } },
      (res) => {
        let buf = '';
        res.setEncoding('utf8');
        res.on('data', (chunk: string) => {
          buf += chunk;
          let idx2 = buf.indexOf('\n\n');
          while (idx2 >= 0) {
            const block = buf.slice(0, idx2);
            buf = buf.slice(idx2 + 2);
            const name = /^event: (.+)$/m.exec(block)?.[1] ?? '';
            const data = /^data: (.+)$/m.exec(block)?.[1] ?? '';
            if (name !== '') onEvent(name, data);
            idx2 = buf.indexOf('\n\n');
          }
        });
        res.on('error', () => {});
        resolve({ close: () => { req.destroy(); res.destroy(); } });
      },
    );
    req.on('error', reject);
    req.end();
  });
}

// fireReport 冒充 shell，向门户送一条 report_result。
async function fireReport(portalAddr: string, input: unknown): Promise<{ accepted: boolean }> {
  const ws = new WebSocket(`ws://${portalAddr}/ws`);
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => { resolve(); });
    ws.once('error', reject);
  });
  const env = encode({
    v: 1,
    id: 'r1',
    type: 'jump',
    from: 'shell',
    to: 'frontend',
    payload: { action: 'report_result', input },
  });
  const reply = await new Promise<{ ok?: boolean; data?: { accepted?: boolean }; error?: string }>((resolve, reject) => {
    ws.once('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      const env2 = decode(bytes) as { payload?: { ok?: boolean; data?: { accepted?: boolean }; error?: string } };
      resolve(env2.payload ?? {});
    });
    ws.once('error', reject);
    ws.send(env);
  });
  ws.terminate();
  return { accepted: reply.data?.accepted === true };
}

test('terminal：命令交给 shell，受理回执带回 item_id', async (t) => {
  const stub = await startStub({ ok: true, data: { item_id: 'sh-1', phase: 'queued' } });
  t.after(stub.stop);
  const { handle, stub: center } = await startPortal([{ name: 'shell', addr: stub.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const res = await postJSON(`http://${handle.addr}/api/terminal`, { command: 'echo hi' });
  assert.equal(res.status, 200);
  const body = (await res.json()) as { itemId: string; phase: string };
  assert.equal(body.itemId, 'sh-1');
  assert.equal(body.phase, 'queued');
  const sent = stub.jumps.at(-1);
  assert.equal(sent?.to, 'shell');
  assert.equal(sent?.action, 'run');
  assert.deepEqual(sent?.input, { command: 'echo hi' });
  // boss 必须是门户自己：shell 的补报要靠它找回来路。
  assert.equal(sent?.boss, 'frontend');
});

test('terminal：缺 command 400；空 item_id 502', async (t) => {
  const stub = await startStub({ ok: true, data: {} });
  t.after(stub.stop);
  const { handle, stub: center } = await startPortal([{ name: 'shell', addr: stub.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const missing = await postJSON(`http://${handle.addr}/api/terminal`, { command: '  ' });
  assert.equal(missing.status, 400);

  // 名册里有 shell，但它回了个没有 item_id 的回执：不能当成功。
  const noId = await postJSON(`http://${handle.addr}/api/terminal`, { command: 'echo hi' });
  assert.equal(noId.status, 502);
});

test('terminal：shell 不在名册（下游不可达）→ 503', async (t) => {
  const stub = await startStub({ ok: true, data: {} });
  t.after(stub.stop);
  const { handle, stub: center } = await startPortal([{ name: 'reasoner', addr: stub.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });
  const res = await postJSON(`http://${handle.addr}/api/terminal`, { command: 'echo hi' });
  assert.equal(res.status, 503);
  assert.match(((await res.json()) as { error: string }).error, /未知目标服务/);
});

test('terminal：report_result 经 SSE 推给浏览器，形状坏的被拒', async (t) => {
  const stub = await startStub({ ok: true, data: { item_id: 'sh-2', phase: 'queued' } });
  t.after(stub.stop);
  const { handle, stub: center } = await startPortal([{ name: 'shell', addr: stub.url }, { name: 'memory', addr: stub.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const events: { name: string; data: string }[] = [];
  const sse = await openSSE(handle.addr, (name, data) => {
    events.push({ name, data });
  });
  t.after(() => {
    sse.close();
  });
  await waitFor(() => events.some((e) => e.name === 'status'), 3000, 'SSE 建立');

  // 形状不对：没有 item_id → 不认领，也不该产生 terminal 事件。
  const bad = await fireReport(handle.addr, { phase: 'done' });
  assert.equal(bad.accepted, false);
  assert.equal(events.filter((e) => e.name === 'terminal').length, 0);

  const good = await fireReport(handle.addr, {
    item_id: 'sh-2',
    phase: 'done',
    text: '执行完成',
    final: true,
    data: { command: 'echo hi', exit_code: 0, stdout: 'hi\n', stderr: '', duration_ms: 12 },
  });
  assert.equal(good.accepted, true);
  await waitFor(() => events.some((e) => e.name === 'terminal'), 3000, 'terminal 事件');
  const update = JSON.parse(events.find((e) => e.name === 'terminal')?.data ?? '{}') as {
    itemId: string;
    phase: string;
    stdout: string;
    exitCode: number;
    final: boolean;
  };
  assert.equal(update.itemId, 'sh-2');
  assert.equal(update.phase, 'done');
  assert.equal(update.stdout, 'hi\n');
  assert.equal(update.exitCode, 0);
  assert.equal(update.final, true);

  // 终态要进会话记录（终端与 agent 共享同一个工作现场）。
  await waitFor(
    () => stub.jumps.some((j) => j.to === 'memory' && j.action === 'store'),
    3000,
    '终端结果入账',
  );
  const store = stub.jumps.find((j) => j.to === 'memory' && j.action === 'store');
  const input = store?.input as { kind: string; role: string; content: string; conversation_id: string };
  assert.equal(input.kind, 'tool_result');
  assert.equal(input.role, 'tool');
  assert.equal(input.conversation_id, 'web:private:owner');
  assert.match(input.content, /^\$ echo hi\nhi/);
});
