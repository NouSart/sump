// HTTP API 测试：overview / message / set / reset / SSE / 静态资源 / 错误路径（含中心不可达）+ 信任栅栏。
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

async function makeWebDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'sump-web-'));
  await writeFile(join(dir, 'index.html'), '<!doctype html><title>面板</title><p>ok</p>', 'utf8');
  return dir;
}

async function startWithCenter(centerURL: string, heartbeatMS = 50): Promise<FrontendHandle> {
  return startFrontend(
    {
      name: 'frontend',
      addr: '127.0.0.1:0',
      centerURL,
      webDir: await makeWebDir(),
      heartbeatMS,
      reasoner: 'reasoner',
      memory: 'memory',
      approval: 'approval',
      trustedHosts: [],
    },
    silent,
  );
}

// startStubService：冒充一个目标服务（真 WS 端点），记录收到的 jump 并按固定答复回执。
async function startStubService(reply: { ok: boolean; data?: unknown; error?: string }): Promise<{
  url: string;
  jumps: { action: string; input: unknown; from: string; boss: string }[];
  stop: () => Promise<void>;
}> {
  const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
  const jumps: { action: string; input: unknown; from: string; boss: string }[] = [];
  wss.on('connection', (ws) => {
    ws.on('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      const env = decode(bytes) as {
        id: string;
        from: string;
        boss?: string;
        payload?: { action?: string; input?: unknown };
      };
      if (env.payload === undefined) return;
      jumps.push({ action: env.payload.action ?? '', input: env.payload.input, from: env.from, boss: env.boss ?? '' });
      ws.send(encode({ v: 1, id: env.id, type: 'response', from: 'reasoner', to: env.from, payload: reply }));
    });
  });
  await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
  const port = (wss.address() as { port: number }).port;
  return {
    url: `ws://127.0.0.1:${port}/ws`,
    jumps,
    stop: () =>
      new Promise<void>((resolve) => {
        for (const c of wss.clients) c.terminate();
        wss.close(() => resolve());
      }),
  };
}

// rawGet 用 node:http 直发请求：验收栅栏必须能自己指定 Host / Origin（fetch 不让改）。
function rawGet(addr: string, path: string, headers: Record<string, string>): Promise<{ status: number; body: string }> {
  const idx = addr.lastIndexOf(':');
  return new Promise((resolve, reject) => {
    const req = httpRequest(
      { host: addr.slice(0, idx), port: Number(addr.slice(idx + 1)), path, method: 'GET', headers },
      (res) => {
        let body = '';
        res.setEncoding('utf8');
        res.on('data', (c: string) => {
          body += c;
        });
        res.on('end', () => resolve({ status: res.statusCode ?? 0, body }));
      },
    );
    req.on('error', reject);
    req.end();
  });
}

interface OverviewBody {
  connected: boolean;
  center: string;
  self: { name: string; ws: string; http: string };
  roster: { services: { name: string }[]; revision: number } | null;
  settings: { services: { service: string; settings?: { key: string; value: string; overridden: boolean }[] }[] } | null;
  error?: string;
}

function postJSON(url: string, body: unknown): Promise<Response> {
  return fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

test('overview：连接状态 + 名册 + 设置清单', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/ws' },
      { name: 'reasoner', addr: 'ws://127.0.0.1:8/ws', provides: [{ action: 'user_message' }] },
    ],
    revision: 5,
  };
  stub.settingsReply = {
    services: [
      {
        service: 'reasoner',
        settings: [{ key: 'tool.notify_mode', value: 'each', default: 'each', overridden: false }],
      },
    ],
  };
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/overview`);
  assert.equal(res.status, 200);
  const body = (await res.json()) as OverviewBody;
  assert.equal(body.connected, true);
  assert.equal(body.center, centerURL);
  assert.equal(body.self.name, 'frontend');
  assert.equal(body.roster?.revision, 5);
  assert.equal(body.roster?.services.length, 2);
  assert.equal(body.settings?.services[0]?.service, 'reasoner');
  assert.equal(body.settings?.services[0]?.settings?.[0]?.key, 'tool.notify_mode');
});

test('set / reset：转发为中心动作并回传更新', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const setRes = await postJSON(`http://${handle.addr}/api/settings/set`, {
    service: 'reasoner',
    key: 'tool.notify_mode',
    value: 'batch',
  });
  assert.equal(setRes.status, 200);
  const setBody = (await setRes.json()) as { ok: boolean; services: { service: string; settings?: { value: string }[] }[] };
  assert.equal(setBody.ok, true);
  assert.equal(setBody.services[0]?.settings?.[0]?.value, 'batch');
  const setJump = stub.jumps.at(-1);
  assert.equal(setJump?.action, 'set_setting');
  assert.deepEqual(setJump?.input, { service: 'reasoner', key: 'tool.notify_mode', value: 'batch' });

  const resetRes = await postJSON(`http://${handle.addr}/api/settings/reset`, {
    service: 'reasoner',
    key: 'tool.notify_mode',
  });
  assert.equal(resetRes.status, 200);
  const resetJump = stub.jumps.at(-1);
  assert.equal(resetJump?.action, 'reset_setting');
  assert.deepEqual(resetJump?.input, { service: 'reasoner', key: 'tool.notify_mode' });
});

test('中心拒绝 → 400；中心不可达 → 503；overview 如实报告', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  stub.setError = '设置项未声明';
  const rejected = await postJSON(`http://${handle.addr}/api/settings/set`, { service: 's', key: 'k', value: 'v' });
  assert.equal(rejected.status, 400);
  const rejectedBody = (await rejected.json()) as { error: string };
  assert.match(rejectedBody.error, /设置项未声明/);

  await stub.stop();
  await waitFor(() => !handle.link.connected, 3000, '中心断开');

  const offline = await postJSON(`http://${handle.addr}/api/settings/set`, { service: 's', key: 'k', value: 'v' });
  assert.equal(offline.status, 503);

  const overview = await fetch(`http://${handle.addr}/api/overview`);
  assert.equal(overview.status, 200);
  const body = (await overview.json()) as OverviewBody;
  assert.equal(body.connected, false);
  assert.equal(body.settings, null);
  assert.equal(body.error, '未连接到设置中心');
});

test('中心不可达（启动时即不可达）：面板仍可用并如实报告', async (t) => {
  const handle = await startWithCenter('ws://127.0.0.1:1/ws', 50);
  t.after(() => handle.close());

  const overview = await fetch(`http://${handle.addr}/api/overview`);
  assert.equal(overview.status, 200);
  const body = (await overview.json()) as OverviewBody;
  assert.equal(body.connected, false);

  const res = await postJSON(`http://${handle.addr}/api/settings/set`, { service: 's', key: 'k', value: 'v' });
  assert.equal(res.status, 503);
});

test('参数校验与未知接口', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });

  const empty = await postJSON(`http://${handle.addr}/api/settings/set`, {});
  assert.equal(empty.status, 400);

  const badJSON = await fetch(`http://${handle.addr}/api/settings/set`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: '{不是 JSON',
  });
  assert.equal(badJSON.status, 400);

  // 跨站简单请求（text/plain）不带预检，必须被 Content-Type 闸挡下。
  const plainText = await fetch(`http://${handle.addr}/api/message`, {
    method: 'POST',
    headers: { 'content-type': 'text/plain' },
    body: JSON.stringify({ text: '你好' }),
  });
  assert.equal(plainText.status, 415);

  const wrongMethod = await fetch(`http://${handle.addr}/api/settings/set`);
  assert.equal(wrongMethod.status, 405);

  const unknown = await fetch(`http://${handle.addr}/api/nope`);
  assert.equal(unknown.status, 404);
});

test('静态资源：根路径 / SPA 回退 / 未知扩展名 404 / 目录穿越 403', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });

  const root = await fetch(`http://${handle.addr}/`);
  assert.equal(root.status, 200);
  assert.match(root.headers.get('content-type') ?? '', /text\/html/);
  assert.match(await root.text(), /面板/);

  const spa = await fetch(`http://${handle.addr}/some/route`);
  assert.equal(spa.status, 200);

  const missingAsset = await fetch(`http://${handle.addr}/logo.svg`);
  assert.equal(missingAsset.status, 404);

  const traversal = await fetch(`http://${handle.addr}/%2e%2e%2f%2e%2e%2fsecret.txt`);
  assert.equal(traversal.status, 403);
});

test('SSE：初始 status/roster + 名册广播实时推送', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  const ctrl = new AbortController();
  t.after(async () => {
    ctrl.abort();
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/events`, { signal: ctrl.signal });
  assert.equal(res.status, 200);
  assert.match(res.headers.get('content-type') ?? '', /text\/event-stream/);
  const reader = res.body?.getReader();
  assert.ok(reader, 'SSE 响应应有 body');
  const decoder = new TextDecoder();

  let buf = '';
  const readEvents = async (n: number): Promise<string[]> => {
    const events: string[] = [];
    while (events.length < n) {
      const { value, done } = await reader.read();
      if (done) break;
      buf += decoder.decode(value, { stream: true });
      let idx = buf.indexOf('\n\n');
      while (idx >= 0) {
        events.push(buf.slice(0, idx));
        buf = buf.slice(idx + 2);
        idx = buf.indexOf('\n\n');
      }
    }
    return events;
  };

  const initial = await readEvents(2);
  assert.match(initial[0] ?? '', /event: status/);
  assert.match(initial[1] ?? '', /event: roster/);

  stub.broadcastRoster({ services: [{ name: 'svc-x', addr: 'ws://127.0.0.1:7/ws' }], revision: 9 });
  const next = await readEvents(1);
  assert.match(next[0] ?? '', /event: roster/);
  assert.match(next[0] ?? '', /svc-x/);

  // settings 事件：中心广播的设置下发结果同样实时转发。
  stub.broadcastSettings({
    cause: 'set_setting',
    service: 'svc-x',
    results: [{ key: 'tool.notify_mode', value: 'batch', ok: true }],
  });
  const settingsEvents = await readEvents(1);
  assert.match(settingsEvents[0] ?? '', /event: settings/);
  assert.match(settingsEvents[0] ?? '', /set_setting/);
  assert.match(settingsEvents[0] ?? '', /tool\.notify_mode/);
});

// ---------- 门户 ----------

test('门户：注册载荷里带着 deliver（名册里看得见）', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const reg = stub.registrations[0] as { provides?: { action: string }[] } | undefined;
  const actions = reg?.provides?.map((p) => p.action) ?? [];
  assert.ok(actions.includes('deliver'), `注册载荷应含 deliver，实际: ${JSON.stringify(actions)}`);
});

test('门户：注册名片里声明五个只读设置项（参数进面板看得见，但不接受下发）', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });

  await waitFor(() => stub.registrations.length > 0, 3000, '注册到达');
  const reg = stub.registrations[0] as { name?: string; settings?: { key?: string; default?: string; readonly?: boolean }[] };
  assert.equal(reg.name, 'frontend');
  const items = reg.settings ?? [];
  assert.equal(items.length, 5, `应声明 5 项，实际 ${JSON.stringify(items)}`);
  const byKey = new Map(items.map((it) => [it.key ?? '', it]));
  for (const [key, want] of [
    ['name', 'frontend'],
    ['reasoner', 'reasoner'],
  ] as const) {
    const it = byKey.get(key);
    assert.equal(it?.readonly, true, `${key} 应是只读项`);
    assert.equal(it?.default, want, `${key} 的默认值应是启动参数`);
  }
  // addr / center / web 是运行期才知道值（端口 0 → 实际端口；web 是临时目录）
  for (const key of ['addr', 'center', 'web']) {
    assert.equal(byKey.get(key)?.readonly, true, `${key} 应是只读项`);
  }
  assert.match(byKey.get('center')?.default ?? '', /^ws:\/\/127\.0\.0\.1:\d+\/ws$/);
  assert.match(byKey.get('addr')?.default ?? '', /^127\.0\.0\.1:\d+$/);
});

test('门户：GET /api/history 直连 memory 取历史（中心只给地址，不转发）', async (t) => {
  const memory = await startStubService({
    ok: true,
    data: {
      conversation_id: 'web:private:owner',
      messages: [
        { role: 'user', content: '上一轮的问题', at: '2026-10-17T10:00:00Z' },
        { role: 'assistant', content: '上一轮的答复', at: '2026-10-17T10:00:02Z' },
      ],
    },
  });
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/ws' },
      { name: 'memory', addr: memory.url },
    ],
    revision: 1,
  };
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
    await memory.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/history?limit=7`);
  assert.equal(res.status, 200);
  const body = (await res.json()) as { conversationId?: string; messages?: { role?: string; content?: string }[] };
  assert.equal(body.conversationId, 'web:private:owner');
  assert.equal(body.messages?.length, 2);
  assert.equal(body.messages?.[0]?.content, '上一轮的问题');

  assert.equal(memory.jumps.length, 1, '应恰好跳一次');
  assert.equal(memory.jumps[0]?.action, 'history');
  assert.deepEqual(memory.jumps[0]?.input, { conversation_id: 'web:private:owner', limit: 7, after_seq: 0 });

  // 增量拉取：after_seq 原样带给 memory（门户不自己判断"哪些是新的"）
  await fetch(`http://${handle.addr}/api/history?limit=9&after_seq=12`);
  assert.deepEqual(memory.jumps[1]?.input, { conversation_id: 'web:private:owner', limit: 9, after_seq: 12 });
});

test('门户：GET /api/history 目标不在名册里 → 明确的错误而不是空列表', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = { services: [{ name: 'frontend', addr: 'ws://127.0.0.1:9/ws' }], revision: 1 };
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/history`);
  assert.notEqual(res.status, 200);
  const body = (await res.json()) as { error?: string };
  assert.match(body.error ?? '', /memory/);
});
test('门户：GET /api/tasks 直连 memory 列出任务', async (t) => {
  const memory = await startStubService({
    ok: true,
    data: { tasks: [{ id: 't1', name: '写测试', status: 'active' }] },
  });
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/ws' },
      { name: 'memory', addr: memory.url },
    ],
    revision: 1,
  };
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
    await memory.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/tasks`);
  assert.equal(res.status, 200);
  const body = (await res.json()) as { conversationId?: string; tasks?: { id?: string; name?: string }[] };
  assert.equal(body.conversationId, 'web:private:owner');
  assert.equal(body.tasks?.length, 1);
  assert.equal(body.tasks?.[0]?.id, 't1');

  assert.equal(memory.jumps.length, 1, '应恰好跳一次');
  assert.equal(memory.jumps[0]?.action, 'task_list');
  assert.deepEqual(memory.jumps[0]?.input, { conversation_id: 'web:private:owner' });
});

test('门户：POST /api/tasks/resume 恢复任务并带回记录', async (t) => {
  const memory = await startStubService({
    ok: true,
    data: {
      task: { id: 't1', name: '写测试', status: 'active' },
      messages: [{ role: 'user', content: '之前的消息' }],
    },
  });
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/ws' },
      { name: 'memory', addr: memory.url },
    ],
    revision: 1,
  };
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
    await memory.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await postJSON(`http://${handle.addr}/api/tasks/resume`, { task_id: 't1' });
  assert.equal(res.status, 200);
  const body = (await res.json()) as {
    task?: { id?: string; status?: string };
    messages?: { content?: string }[];
  };
  assert.equal(body.task?.id, 't1');
  assert.equal(body.task?.status, 'active');
  assert.equal(body.messages?.[0]?.content, '之前的消息');
  assert.equal(memory.jumps[0]?.action, 'task_resume');
  assert.deepEqual(memory.jumps[0]?.input, { task_id: 't1' });

  // 缺 task_id：门户自己拦下（400），不打扰 memory。
  const before = memory.jumps.length;
  const bad = await postJSON(`http://${handle.addr}/api/tasks/resume`, {});
  assert.equal(bad.status, 400);
  const badBody = (await bad.json()) as { error?: string };
  assert.match(badBody.error ?? '', /task_id/);
  assert.equal(memory.jumps.length, before, '参数不合法时不应跳转');
});

test('语言包：列目录 / 按语言读取 / 非法标识 400', async (t) => {
  const locales = await mkdtemp(join(tmpdir(), 'sump-locales-'));
  await writeFile(
    join(locales, 'zh-CN.json'),
    JSON.stringify({ locale: 'zh-CN', name: '中文', messages: { hello: '你好' } }),
    'utf8',
  );
  await writeFile(
    join(locales, 'xx-XX.json'),
    JSON.stringify({ locale: 'xx-XX', name: '全 x', messages: { hello: 'x' } }),
    'utf8',
  );
  const stub = new StubCenter();
  const centerURL = await stub.start();
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
      localesDir: locales,
      lang: 'zh-CN',
      trustedHosts: [],
    },
    silent,
  );
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const listRes = await fetch(`http://${handle.addr}/api/i18n/locales`);
  assert.equal(listRes.status, 200);
  const list = (await listRes.json()) as { locales?: { locale?: string; name?: string }[] };
  const localesSeen = (list.locales ?? []).map((l) => l.locale).sort();
  assert.deepEqual(localesSeen, ['xx-XX', 'zh-CN']);
  assert.equal(list.locales?.find((l) => l.locale === 'xx-XX')?.name, '全 x');

  const packRes = await fetch(`http://${handle.addr}/api/i18n?lang=xx-XX`);
  assert.equal(packRes.status, 200);
  const pack = (await packRes.json()) as { locale?: string; messages?: Record<string, string> };
  assert.equal(pack.locale, 'xx-XX');
  assert.equal(pack.messages?.hello, 'x');

  // 缺包不致命：返回空消息，让界面退回内置文案。
  const missing = (await (await fetch(`http://${handle.addr}/api/i18n?lang=fr-FR`)).json()) as {
    messages?: Record<string, string>;
  };
  assert.deepEqual(missing.messages, {});

  // 语言标识只认 [A-Za-z0-9_-]，挡住目录穿越。
  const bad = await fetch(`http://${handle.addr}/api/i18n?lang=${encodeURIComponent('../zh-CN')}`);
  assert.equal(bad.status, 400);
});

test('门户：POST /api/message 跳 user_message 给 reasoner，只回受理回执', async (t) => {
  const reasoner = await startStubService({ ok: true, data: { item_id: 'h1', phase: 'queued' } });
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/ws' },
      { name: 'reasoner', addr: reasoner.url },
    ],
    revision: 1,
  };
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
    await reasoner.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await postJSON(`http://${handle.addr}/api/message`, { text: '你好' });
  assert.equal(res.status, 200);
  assert.deepEqual(await res.json(), { ok: true, conversationId: 'web:private:owner' });

  assert.equal(reasoner.jumps.length, 1, '应恰好跳一次');
  assert.equal(reasoner.jumps[0]?.action, 'user_message');
  assert.deepEqual(reasoner.jumps[0]?.input, { text: '你好', conversation_id: 'web:private:owner' });
  // boss 必须是门户自己：answer 由 reasoner 经 deliver 直达 boss，少了它回答就没有回来的路
  assert.equal(reasoner.jumps[0]?.from, 'frontend');
  assert.equal(reasoner.jumps[0]?.boss, 'frontend');
});

test('门户：deliver 从 WS 端点进来 → 回执 + SSE 推给浏览器', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  const ctrl = new AbortController();
  t.after(async () => {
    ctrl.abort();
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  // 先订阅再投递，否则事件赶在订阅之前。
  const res = await fetch(`http://${handle.addr}/api/events`, { signal: ctrl.signal });
  const reader = res.body?.getReader();
  assert.ok(reader, 'SSE 响应应有 body');
  const decoder = new TextDecoder();
  let buf = '';
  const readEvent = async (): Promise<string> => {
    for (;;) {
      const idx = buf.indexOf('\n\n');
      if (idx >= 0) {
        const ev = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        return ev;
      }
      const { value, done } = await reader.read();
      if (done) throw new Error('SSE 提前结束');
      buf += decoder.decode(value, { stream: true });
    }
  };
  await readEvent(); // status
  await readEvent(); // roster
  await readEvent(); // timers（冷启动补的空快照）

  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => ws.terminate());
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const receipt = new Promise<{ ok: boolean; data?: { delivered?: boolean } }>((resolve) => {
    ws.on('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      resolve((decode(bytes) as { payload: { ok: boolean; data?: { delivered?: boolean } } }).payload);
    });
  });
  ws.send(
    encode({
      v: 1,
      id: 'deliver-1',
      type: 'jump',
      from: 'reasoner',
      to: 'frontend',
      payload: { action: 'deliver', input: { text: '任务完成' } },
    }),
  );
  const got = await receipt;
  assert.equal(got.ok, true, 'deliver 应被受理');
  assert.equal(got.data?.delivered, true);

  const ev = await readEvent();
  assert.match(ev, /event: deliver/);
  assert.match(ev, /任务完成/);
  assert.match(ev, /web:private:owner/, '未给 conversation_id 时用单用户默认会话');
});

test('门户：progress 从 WS 端点进来 → 回执 + SSE 推给浏览器；形状不对的丢弃', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  const ctrl = new AbortController();
  t.after(async () => {
    ctrl.abort();
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/events`, { signal: ctrl.signal });
  const reader = res.body?.getReader();
  assert.ok(reader, 'SSE 响应应有 body');
  const decoder = new TextDecoder();
  let buf = '';
  const readEvent = async (): Promise<string> => {
    for (;;) {
      const idx = buf.indexOf('\n\n');
      if (idx >= 0) {
        const ev = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        return ev;
      }
      const { value, done } = await reader.read();
      if (done) throw new Error('SSE 提前结束');
      buf += decoder.decode(value, { stream: true });
    }
  };
  await readEvent(); // status
  await readEvent(); // roster
  await readEvent(); // timers

  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => ws.terminate());
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  let resolveReceipt: (v: { ok: boolean; data?: { accepted?: boolean } }) => void = () => {};
  const receipt = new Promise<{ ok: boolean; data?: { accepted?: boolean } }>((resolve) => {
    resolveReceipt = resolve;
  });
  ws.on('message', (data: RawData) => {
    const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
    resolveReceipt((decode(bytes) as { payload: { ok: boolean; data?: { accepted?: boolean } } }).payload);
  });

  // 形状不对：缺 kind → 明确拒绝，且不往 SSE 推
  ws.send(
    encode({
      v: 1,
      id: 'progress-bad',
      type: 'jump',
      from: 'reasoner',
      to: 'frontend',
      payload: { action: 'progress', input: { content: '没有 kind' } },
    }),
  );
  const bad = await receipt;
  assert.equal(bad.ok, true);
  assert.equal(bad.data?.accepted, false, '形状不对的实况应被丢弃');

  const okReceipt = new Promise<{ ok: boolean; data?: { accepted?: boolean } }>((resolve) => {
    ws.once('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      resolve((decode(bytes) as { payload: { ok: boolean; data?: { accepted?: boolean } } }).payload);
    });
  });
  ws.send(
    encode({
      v: 1,
      id: 'progress-1',
      type: 'jump',
      from: 'reasoner',
      to: 'frontend',
      payload: {
        action: 'progress',
        input: {
          conversation_id: 'web:private:owner',
          seq: 7,
          kind: 'tool_call',
          content: 'Get-Date',
          meta: '{"tool":"shell__run","call_id":"c1"}',
          at: '2026-10-17T10:00:03Z',
        },
      },
    }),
  );
  const ok = await okReceipt;
  assert.equal(ok.data?.accepted, true);

  const ev = await readEvent();
  assert.match(ev, /event: progress/);
  assert.match(ev, /tool_call/);
  assert.match(ev, /Get-Date/);
  assert.match(ev, /"seq":7/);
});

test('门户：timer 推来快照 → 回执 + SSE 全量推给浏览器；旧序号的快照被丢弃', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  const ctrl = new AbortController();
  t.after(async () => {
    ctrl.abort();
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const res = await fetch(`http://${handle.addr}/api/events`, { signal: ctrl.signal });
  const reader = res.body?.getReader();
  assert.ok(reader, 'SSE 响应应有 body');
  const decoder = new TextDecoder();
  let buf = '';
  const readEvent = async (): Promise<string> => {
    for (;;) {
      const idx = buf.indexOf('\n\n');
      if (idx >= 0) {
        const ev = buf.slice(0, idx);
        buf = buf.slice(idx + 2);
        return ev;
      }
      const { value, done } = await reader.read();
      if (done) throw new Error('SSE 提前结束');
      buf += decoder.decode(value, { stream: true });
    }
  };
  await readEvent(); // status
  await readEvent(); // roster
  assert.match(await readEvent(), /event: timers\ndata: \[\]/, '冷启动补一条空的计时快照');

  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => ws.terminate());
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const push = (id: string, seq: number): Promise<{ ok: boolean; data?: { accepted?: boolean } }> =>
    new Promise((resolve) => {
      ws.once('message', (data: RawData) => {
        const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
        resolve((decode(bytes) as { payload: { ok: boolean; data?: { accepted?: boolean } } }).payload);
      });
      ws.send(
        encode({
          v: 1,
          id,
          type: 'jump',
          from: 'timer',
          to: 'frontend',
          payload: {
            action: 'timer_state',
            input: {
              boss: 'web:private:owner',
              seq,
              timers: [{ item_id: 'job-1', caller: 'reasoner', ms: 30000, fire_at: '2030-01-01T00:00:00Z', note: '等构建' }],
            },
          },
        }),
      );
    });

  const first = await push('ts-1', 7);
  assert.equal(first.ok, true, 'timer_state 应被受理');
  assert.equal(first.data?.accepted, true);

  const ev = await readEvent();
  assert.match(ev, /event: timers/);
  assert.match(ev, /"itemId":"job-1"/);
  assert.match(ev, /2030-01-01T00:00:00Z/, '倒计时要用到 fire_at');
  assert.match(ev, /等构建/);

  // 乱序到达的迟到快照（序号更小）必须被丢弃——否则倒计时会倒回旧状态。
  const late = await push('ts-2', 6);
  assert.equal(late.data?.accepted, false, '旧序号快照应被丢弃');
});

test('待审命令：approval_pending 推到面板 → 用户裁决经 approval_decide 送回 approval', async (t) => {
  const stub = new StubCenter();
  const stubApproval = await startStubService({ ok: true, data: { status: 'accepted' } });
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
    await stubApproval.stop();
  });
  // 名册里要有 approval（门户要能从名册解析出它的地址去送裁决）。
  stub.roster = { services: [{ name: 'approval', addr: stubApproval.url, provides: [] }], revision: 2 };
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');
  stub.broadcastRoster();
  await waitFor(() => (handle.link.roster?.services ?? []).length > 0, 3000, '名册到达');

  const res = await fetch(`http://${handle.addr}/api/approvals`);
  assert.equal(res.status, 200);
  assert.deepEqual((await res.json()) as { items: unknown[] }, { items: [] }, '初始没有待审项');

  // approval 推一条待审过来。
  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => ws.terminate());
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const pushed = await new Promise<{ ok: boolean; data?: { accepted?: boolean } }>((resolve) => {
    ws.once('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      resolve((decode(bytes) as { payload: { ok: boolean; data?: { accepted?: boolean } } }).payload);
    });
    ws.send(
      encode({
        v: 1,
        id: 'ap-1',
        type: 'jump',
        from: 'approval',
        to: 'frontend',
        payload: {
          action: 'approval_pending',
          input: {
            item_id: 'item-42',
            command: 'Get-Process',
            work_dir: 'C:\\root',
            requested_by: 'shell',
            reason_code: 'NO_MATCHING_RULE',
            summary: '没有覆盖该命令的允许规则',
            created_ms: 1,
            expires_ms: 2,
          },
        },
      }),
    );
  });
  assert.equal(pushed.ok, true, 'approval_pending 应被受理');
  assert.equal(pushed.data?.accepted, true);

  const listed = (await (await fetch(`http://${handle.addr}/api/approvals`)).json()) as {
    items: { itemId: string; command: string; reasonCode: string }[];
  };
  assert.equal(listed.items.length, 1);
  assert.equal(listed.items[0]?.itemId, 'item-42');
  assert.equal(listed.items[0]?.command, 'Get-Process', '命令原文必须给用户看');
  assert.equal(listed.items[0]?.reasonCode, 'NO_MATCHING_RULE');

  // 用户在面板点"批准"→ 门户把裁决送回 approval（真 WS 跳转）。
  const decided = await postJSON(`http://${handle.addr}/api/approvals`, { item_id: 'item-42', decision: 'allow' });
  assert.equal(decided.status, 200);
  const viaApproval = stubApproval.jumps.filter((j) => j.action === 'approval_decide');
  assert.equal(viaApproval.length, 1, '应恰好送出一条裁决');
  assert.deepEqual(viaApproval[0]?.input, { item_id: 'item-42', decision: 'allow', note: '' });
  assert.equal(viaApproval[0]?.from, 'frontend', '裁决必须由门户发出（approval 按 from 校验）');

  const after = (await (await fetch(`http://${handle.addr}/api/approvals`)).json()) as { items: unknown[] };
  assert.equal(after.items.length, 0, '裁决后卡片应消失');
});

test('待审命令：decision 只接受 allow / deny，形状不对的推送被丢弃', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const bad = await postJSON(`http://${handle.addr}/api/approvals`, { item_id: 'x', decision: 'maybe' });
  assert.equal(bad.status, 400);

  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => ws.terminate());
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const receipt = await new Promise<{ ok: boolean; data?: { accepted?: boolean } }>((resolve) => {
    ws.once('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      resolve((decode(bytes) as { payload: { ok: boolean; data?: { accepted?: boolean } } }).payload);
    });
    ws.send(
      encode({
        v: 1,
        id: 'ap-bad',
        type: 'jump',
        from: 'approval',
        to: 'frontend',
        payload: { action: 'approval_pending', input: { item_id: '', command: 'x' } },
      }),
    );
  });
  assert.equal(receipt.ok, true, '坏推送也要回执（不静默）');
  assert.equal(receipt.data?.accepted, false, '缺 item_id 的待审项必须丢弃');
});

test('门户：deliver 缺 text 时明确拒绝（不静默吞掉）', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => ws.terminate());
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const receipt = new Promise<{ ok: boolean; error?: string }>((resolve) => {
    ws.on('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      resolve((decode(bytes) as { payload: { ok: boolean; error?: string } }).payload);
    });
  });
  ws.send(encode({ v: 1, id: 'deliver-2', type: 'jump', from: 'reasoner', to: 'frontend', payload: { action: 'deliver', input: {} } }));
  const got = await receipt;
  assert.equal(got.ok, false);
  assert.match(got.error ?? '', /text/);
});

test('信任栅栏：WS 端点同样拦截带外站 Origin 的握手', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });

  // 恶意页面直连 WS（浏览器握手必带 Origin）→ 应被拒；服务之间的连接不带 Origin → 放行。
  const bad = new WebSocket(`ws://${handle.addr}/ws`, { origin: 'https://evil.example' });
  const badOutcome = await new Promise<string>((resolve) => {
    bad.once('open', () => resolve('opened'));
    bad.once('error', () => resolve('rejected'));
  });
  assert.equal(badOutcome, 'rejected', '带外站 Origin 的 WS 握手应被拒');

  const good = new WebSocket(`ws://${handle.addr}/ws`);
  t.after(() => good.terminate());
  const goodOutcome = await new Promise<string>((resolve) => {
    good.once('open', () => resolve('opened'));
    good.once('error', () => resolve('rejected'));
  });
  assert.equal(goodOutcome, 'opened', '无 Origin 的服务间连接应放行');

  // 沙箱 iframe / file:// 页面的握手带 Origin: null——那不是"没有 Origin"。
  const sandboxed = new WebSocket(`ws://${handle.addr}/ws`, { origin: 'null' });
  const sandboxedOutcome = await new Promise<string>((resolve) => {
    sandboxed.once('open', () => resolve('opened'));
    sandboxed.once('error', () => resolve('rejected'));
  });
  assert.equal(sandboxedOutcome, 'rejected', 'Origin: null 的 WS 握手应被拒');
});

test('信任栅栏：伪造 Host / 跨站 Origin 一律 403，正常回环放行', async (t) => {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  const port = Number(handle.addr.slice(handle.addr.lastIndexOf(':') + 1));

  assert.equal((await rawGet(handle.addr, '/api/overview', { host: handle.addr })).status, 200, '绑定地址应放行');
  assert.equal((await rawGet(handle.addr, '/api/overview', { host: `localhost:${port}` })).status, 200, '回环别名应放行');

  // DNS rebinding：Host 写着攻击者的域名。
  const rebound = await rawGet(handle.addr, '/api/overview', { host: 'evil.example' });
  assert.equal(rebound.status, 403);
  assert.match(rebound.body, /Host 不在受信范围/);

  // 跨站：Host 对，但 Origin 是别人的页面。
  const crossSite = await rawGet(handle.addr, '/api/overview', { host: handle.addr, origin: 'https://evil.example' });
  assert.equal(crossSite.status, 403);
  assert.match(crossSite.body, /Origin 不在受信范围/);

  // 沙箱 iframe / file:// 页面：Origin 序列化成字符串 'null'，同样必须拒绝。
  const nullOrigin = await rawGet(handle.addr, '/api/overview', { host: handle.addr, origin: 'null' });
  assert.equal(nullOrigin.status, 403);
  assert.match(nullOrigin.body, /Origin 不受信/);

  // 端口不符：不同端口是另一个服务，没理由放行。
  assert.equal((await rawGet(handle.addr, '/api/overview', { host: `127.0.0.1:${port + 1}` })).status, 403);
});
