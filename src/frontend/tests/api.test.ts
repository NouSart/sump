// HTTP API 测试：overview / message / set / reset / SSE / 静态资源 / 错误路径（含中心不可达）+ 信任栅栏。
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { request as httpRequest } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { decode, encode } from '@msgpack/msgpack';
import { WebSocket } from 'ws';
import { startFrontend, type FrontendHandle } from '../src/server/app.ts';
import { SPv2RemoteError, decodeFrame, dialSPv2Once, encodeFrame } from '../src/server/spv2.ts';
import { newID } from '../src/server/protocol.ts';
import { StubNativeCenter } from './stub-native-center.ts';
import { StubNativeService, type NativeCall } from './stub-native-service.ts';
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

// startStubService：冒充一个目标服务（真 /spv2 端点），记录收到的 call 并按固定答复回执。
async function startStubService(reply: { ok: boolean; data?: unknown; error?: string }): Promise<{
  url: string;
  calls: NativeCall[];
  stop: () => Promise<void>;
}> {
  const stub = new StubNativeService();
  stub.replyFor = () => reply;
  const url = await stub.start();
  return { url, calls: stub.calls, stop: () => stub.stop() };
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

// rawDeliver 发一条完整原生 deliver（自定义 from.authority）：归属校验必须看到 runtime。
async function rawDeliver(
  addr: string,
  from: { kind: string; id: string; authority?: string },
  work: string,
  body: Record<string, unknown>,
): Promise<{ status: string; body?: unknown; error?: { code: string; message: string } }> {
  const ws = new WebSocket(`ws://${addr}/spv2`);
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const read = (): Promise<Record<string, unknown>> =>
    new Promise((resolve) => {
      ws.once("message", (data) => resolve(decodeFrame(new Uint8Array(data as Buffer)) as unknown as Record<string, unknown>));
    });
  const hello = {
    v: 'spv2.1', id: newID(), type: 'call', work: newID(),
    from: { kind: 'service', id: 'frontend' }, to: { kind: 'service', id: 'frontend' },
    method: 'hello', content_type: 'application/msgpack', body: encode({ protocol: 'spv2.1' }),
  };
  ws.send(encodeFrame(hello as never));
  await read();
  const call = {
    v: 'spv2.1', id: newID(), type: 'call', work,
    from, to: { kind: 'service', id: 'frontend' },
    method: 'deliver', content_type: 'application/msgpack', body: encode(body),
  };
  ws.send(encodeFrame(call as never));
  const reply = await read();
  ws.close();
  const rawBody = reply.body;
  if (rawBody instanceof Uint8Array && rawBody.length > 0) {
    reply.body = decode(rawBody);
  }
  return reply as { status: string; body?: unknown; error?: { code: string; message: string } };
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
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/spv2' },
      { name: 'reasoner', addr: 'ws://127.0.0.1:8/spv2', provides: [{ action: 'message' }] },
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
  assert.equal(body.self.ws, `ws://${handle.addr}/spv2`, '门户对外只公布 /spv2 端点');
  assert.equal(body.roster?.revision, 5);
  assert.equal(body.roster?.services.length, 2);
  assert.equal(body.settings?.services[0]?.service, 'reasoner');
  assert.equal(body.settings?.services[0]?.settings?.[0]?.key, 'tool.notify_mode');
});

test('set / reset：转发为中心动作并回传更新', async (t) => {
  const stub = new StubNativeCenter();
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
  const setCall = stub.calls.at(-1);
  assert.equal(setCall?.method, 'set_setting');
  assert.deepEqual(setCall?.input, { service: 'reasoner', key: 'tool.notify_mode', value: 'batch' });

  const resetRes = await postJSON(`http://${handle.addr}/api/settings/reset`, {
    service: 'reasoner',
    key: 'tool.notify_mode',
  });
  assert.equal(resetRes.status, 200);
  const resetCall = stub.calls.at(-1);
  assert.equal(resetCall?.method, 'reset_setting');
  assert.deepEqual(resetCall?.input, { service: 'reasoner', key: 'tool.notify_mode' });
});

test('中心拒绝 → 400；中心不可达 → 503；overview 如实报告', async (t) => {
  const stub = new StubNativeCenter();
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
  const handle = await startWithCenter('ws://127.0.0.1:1/spv2', 50);
  t.after(() => handle.close());

  const overview = await fetch(`http://${handle.addr}/api/overview`);
  assert.equal(overview.status, 200);
  const body = (await overview.json()) as OverviewBody;
  assert.equal(body.connected, false);

  const res = await postJSON(`http://${handle.addr}/api/settings/set`, { service: 's', key: 'k', value: 'v' });
  assert.equal(res.status, 503);
});

test('参数校验与未知接口', async (t) => {
  const stub = new StubNativeCenter();
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
  const stub = new StubNativeCenter();
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
  const stub = new StubNativeCenter();
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

  stub.broadcastRoster({ services: [{ name: 'svc-x', addr: 'ws://127.0.0.1:7/spv2' }], revision: 9 });
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
  const stub = new StubNativeCenter();
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
  const stub = new StubNativeCenter();
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
  assert.match(byKey.get('center')?.default ?? '', /^ws:\/\/127\.0\.0\.1:\d+\/spv2$/);
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
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/spv2' },
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

  assert.equal(memory.calls.length, 1, '应恰好跳一次');
  assert.equal(memory.calls[0]?.method, 'history');
  assert.deepEqual(memory.calls[0]?.input, { conversation_id: 'web:private:owner', limit: 7, after_seq: 0 });

  // 增量拉取：after_seq 原样带给 memory（门户不自己判断"哪些是新的"）
  await fetch(`http://${handle.addr}/api/history?limit=9&after_seq=12`);
  assert.deepEqual(memory.calls[1]?.input, { conversation_id: 'web:private:owner', limit: 9, after_seq: 12 });
});

test('门户：GET /api/history 目标不在名册里 → 明确的错误而不是空列表', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  stub.roster = { services: [{ name: 'frontend', addr: 'ws://127.0.0.1:9/spv2' }], revision: 1 };
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
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/spv2' },
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

  assert.equal(memory.calls.length, 1, '应恰好跳一次');
  assert.equal(memory.calls[0]?.method, 'task_list');
  assert.deepEqual(memory.calls[0]?.input, { conversation_id: 'web:private:owner' });
});

test('门户：POST /api/tasks/resume 恢复任务并带回记录', async (t) => {
  const memory = await startStubService({
    ok: true,
    data: {
      task: { id: 't1', name: '写测试', status: 'active' },
      messages: [{ role: 'user', content: '之前的消息' }],
    },
  });
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/spv2' },
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
  assert.equal(memory.calls[0]?.method, 'task_resume');
  assert.deepEqual(memory.calls[0]?.input, { task_id: 't1' });

  // 缺 task_id：门户自己拦下（400），不打扰 memory。
  const before = memory.calls.length;
  const bad = await postJSON(`http://${handle.addr}/api/tasks/resume`, {});
  assert.equal(bad.status, 400);
  const badBody = (await bad.json()) as { error?: string };
  assert.match(badBody.error ?? '', /task_id/);
  assert.equal(memory.calls.length, before, '参数不合法时不应跳转');
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
  const stub = new StubNativeCenter();
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

test('门户：POST /api/message 跳 message 给 reasoner，只回受理回执', async (t) => {
  const reasoner = await startStubService({ ok: true, data: { item_id: 'h1', phase: 'queued' } });
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [
      { name: 'frontend', addr: 'ws://127.0.0.1:9/spv2' },
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

  assert.equal(reasoner.calls.length, 1, '应恰好跳一次');
  assert.equal(reasoner.calls[0]?.method, 'message');
  assert.deepEqual(reasoner.calls[0]?.input, {
    text: '你好',
    conversation: 'web:private:owner',
    sender: { channel: 'private', owner: true },
  });
  // from 必须是门户自己：原生链路里回答沿 from 回来，少了它回答就没有回程
  assert.equal(reasoner.calls[0]?.from, 'frontend');
});

test('门户：deliver 从 WS 端点进来 → 回执 + SSE 推给浏览器', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  const ctrl = new AbortController();
  t.after(async () => {
    ctrl.abort();
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  // spec §6.5：deliver 要先问 memory 的 work 行；这里起一个记忆替身并让它进名册。
  const mem = new StubNativeService();
  mem.replyFor = (method) =>
    method === 'work_get'
      ? { ok: true, data: { work: { agent: { kind: 'agent', id: 'reasoner' }, owner: { kind: 'human', id: 'owner' } } } }
      : { ok: false, error: 'unknown' };
  const memURL = await mem.start();
  t.after(async () => {
    await mem.stop();
  });
  stub.roster = { services: [{ name: 'memory', addr: memURL }], revision: 9 };
  stub.broadcastRoster();
  await waitFor(() => (handle.link.roster?.services ?? []).some((x) => x.name === 'memory'), 3000, 'memory 入名册');

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

  // 原生 call：from=agent:reasoner + authority=runtime（spec §6.5 允许的 deliver 来源）。
  const got = await rawDeliver(handle.addr, { kind: 'agent', id: 'reasoner', authority: 'runtime' }, 'w-deliver', { text: '任务完成' });
  assert.equal(got.status, 'ok', JSON.stringify(got));
  assert.deepEqual(got.body, { delivered: true });

  const ev = await readEvent();
  assert.match(ev, /event: deliver/);
  assert.match(ev, /任务完成/);
  assert.match(ev, /web:private:owner/, '未给 conversation_id 时用单用户默认会话');
});

test('门户：deliver 来源不是 work.agent/owner → permission_denied（不静默放行）', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');
  const mem = new StubNativeService();
  mem.replyFor = () => ({ ok: true, data: { work: { agent: { kind: 'agent', id: 'reasoner' }, owner: { kind: 'human', id: 'owner' } } } });
  const memURL = await mem.start();
  t.after(async () => {
    await mem.stop();
  });
  stub.roster = { services: [{ name: 'memory', addr: memURL }], revision: 9 };
  stub.broadcastRoster();
  await waitFor(() => (handle.link.roster?.services ?? []).some((x) => x.name === 'memory'), 3000, 'memory 入名册');
  // agent 但 authority=none：不能 deliver（runtime 不是随便写的）。
  const denied = await rawDeliver(handle.addr, { kind: 'agent', id: 'reasoner', authority: 'none' }, 'w-deliver', { text: 'x' });
  assert.equal(denied.status, 'error');
  assert.equal(denied.error?.code, 'permission_denied');
  // 冒充 work.agent 的另一个 agent 也不行。
  const impostor = await rawDeliver(handle.addr, { kind: 'agent', id: 'intruder', authority: 'runtime' }, 'w-deliver', { text: 'x' });
  assert.equal(impostor.error?.code, 'permission_denied');
});
test('门户：progress 从 WS 端点进来 → 回执 + SSE 推给浏览器；形状不对的丢弃', async (t) => {
  const stub = new StubNativeCenter();
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

  // 形状不对：缺 kind → 明确拒绝，且不往 SSE 推
  const bad = (await dialSPv2Once(`ws://${handle.addr}/spv2`, 'reasoner', 'frontend', 'progress', { content: '没有 kind' }, 2000)) as {
    accepted?: boolean;
  };
  assert.equal(bad.accepted, false, '形状不对的实况应被丢弃');

  const ok = (await dialSPv2Once(
    `ws://${handle.addr}/spv2`,
    'reasoner',
    'frontend',
    'progress',
    {
      conversation_id: 'web:private:owner',
      seq: 7,
      kind: 'tool_call',
      content: 'Get-Date',
      meta: '{"tool":"shell__run","call_id":"c1"}',
      at: '2026-10-17T10:00:03Z',
    },
    2000,
  )) as { accepted?: boolean };
  assert.equal(ok.accepted, true);

  const ev = await readEvent();
  assert.match(ev, /event: progress/);
  assert.match(ev, /tool_call/);
  assert.match(ev, /Get-Date/);
  assert.match(ev, /"seq":7/);
});

test('门户：timer 推来快照 → 回执 + SSE 全量推给浏览器；旧序号的快照被丢弃', async (t) => {
  const stub = new StubNativeCenter();
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

  const push = async (seq: number): Promise<{ accepted?: boolean }> =>
    (await dialSPv2Once(
      `ws://${handle.addr}/spv2`,
      'timer',
      'frontend',
      'timer_state',
      {
        boss: 'web:private:owner',
        seq,
        timers: [{ item_id: 'job-1', caller: 'reasoner', ms: 30000, fire_at: '2030-01-01T00:00:00Z', note: '等构建' }],
      },
      2000,
    )) as { accepted?: boolean };

  const first = await push(7);
  assert.equal(first.accepted, true, 'timer_state 应被受理');

  const ev = await readEvent();
  assert.match(ev, /event: timers/);
  assert.match(ev, /"itemId":"job-1"/);
  assert.match(ev, /2030-01-01T00:00:00Z/, '倒计时要用到 fire_at');
  assert.match(ev, /等构建/);

  // 乱序到达的迟到快照（序号更小）必须被丢弃——否则倒计时会倒回旧状态。
  const late = await push(6);
  assert.equal(late.accepted, false, '旧序号快照应被丢弃');
});

test('待审命令：approval_pending 推到面板 → 用户裁决经 approval_decide 送回 approval', async (t) => {
  const stub = new StubNativeCenter();
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
  const pushed = (await dialSPv2Once(
    `ws://${handle.addr}/spv2`,
    'approval',
    'frontend',
    'approval_pending',
    {
      item_id: 'item-42',
      command: 'Get-Process',
      work_dir: 'C:\\root',
      requested_by: 'shell',
      reason_code: 'NO_MATCHING_RULE',
      summary: '没有覆盖该命令的允许规则',
      created_ms: 1,
      expires_ms: 2,
    },
    2000,
  )) as { accepted?: boolean };
  assert.equal(pushed.accepted, true, 'approval_pending 应被受理');

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
  const viaApproval = stubApproval.calls.filter((j) => j.method === 'approval_decide');
  assert.equal(viaApproval.length, 1, '应恰好送出一条裁决');
  assert.deepEqual(viaApproval[0]?.input, { item_id: 'item-42', decision: 'allow', note: '' });
  assert.equal(viaApproval[0]?.from, 'frontend', '裁决必须由门户发出（approval 按 from 校验）');

  const after = (await (await fetch(`http://${handle.addr}/api/approvals`)).json()) as { items: unknown[] };
  assert.equal(after.items.length, 0, '裁决后卡片应消失');
});

test('待审命令：decision 只接受 allow / deny，形状不对的推送被丢弃', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  const bad = await postJSON(`http://${handle.addr}/api/approvals`, { item_id: 'x', decision: 'maybe' });
  assert.equal(bad.status, 400);

  // 坏推送也要回执（不静默）：原生 call 正常返回（不抛错）才算有回执。
  const receipt = (await dialSPv2Once(
    `ws://${handle.addr}/spv2`,
    'approval',
    'frontend',
    'approval_pending',
    { item_id: '', command: 'x' },
    2000,
  )) as { accepted?: boolean };
  assert.equal(receipt.accepted, false, '缺 item_id 的待审项必须丢弃');
});

test('门户：deliver 缺 text 时明确拒绝（不静默吞掉）', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 3000, '前端接入中心');

  // 原生 error reply 明确拒绝：SPv2RemoteError 带 code + message。
  await assert.rejects(
    dialSPv2Once(`ws://${handle.addr}/spv2`, 'reasoner', 'frontend', 'deliver', {}, 2000),
    (err: unknown) =>
      err instanceof SPv2RemoteError && err.code === 'invalid_argument' && /text/.test(err.message),
    '缺 text 的 deliver 必须明确拒绝',
  );
});

test('信任栅栏：WS 端点同样拦截带外站 Origin 的握手', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });

  // 恶意页面直连 /spv2（浏览器握手必带 Origin）→ 应被拒；服务之间的连接不带 Origin → 放行。
  const bad = new WebSocket(`ws://${handle.addr}/spv2`, { origin: 'https://evil.example' });
  const badOutcome = await new Promise<string>((resolve) => {
    bad.once('open', () => resolve('opened'));
    bad.once('error', () => resolve('rejected'));
  });
  assert.equal(badOutcome, 'rejected', '带外站 Origin 的 WS 握手应被拒');

  const good = new WebSocket(`ws://${handle.addr}/spv2`);
  t.after(() => good.terminate());
  const goodOutcome = await new Promise<string>((resolve) => {
    good.once('open', () => resolve('opened'));
    good.once('error', () => resolve('rejected'));
  });
  assert.equal(goodOutcome, 'opened', '无 Origin 的服务间连接应放行');

  // 沙箱 iframe / file:// 页面的握手带 Origin: null——那不是"没有 Origin"。
  const sandboxed = new WebSocket(`ws://${handle.addr}/spv2`, { origin: 'null' });
  const sandboxedOutcome = await new Promise<string>((resolve) => {
    sandboxed.once('open', () => resolve('opened'));
    sandboxed.once('error', () => resolve('rejected'));
  });
  assert.equal(sandboxedOutcome, 'rejected', 'Origin: null 的 WS 握手应被拒');
});

test('信任栅栏：伪造 Host / 跨站 Origin 一律 403，正常回环放行', async (t) => {
  const stub = new StubNativeCenter();
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

test('观测端点：healthz / readyz / metrics（OpenMetrics）', async (t) => {
  const stub = new StubNativeCenter();
  const centerURL = await stub.start();
  const handle = await startWithCenter(centerURL);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });
  await waitFor(() => handle.link.connected, 5000, '中心连接');

  const health = await rawGet(handle.addr, '/healthz', { host: handle.addr });
  assert.equal(health.status, 200);
  assert.match(health.body, /^ok/);

  const ready = await rawGet(handle.addr, '/readyz', { host: handle.addr });
  assert.equal(ready.status, 200, '中心在线时就绪必须 200');
  assert.equal((JSON.parse(ready.body) as { status: string }).status, 'ok');

  const metrics = await rawGet(handle.addr, '/metrics', { host: handle.addr });
  assert.equal(metrics.status, 200);
  assert.match(metrics.body, /# EOF/);
  assert.match(metrics.body, /sump_requests_total\{method="GET",status="200"\}/);
  assert.match(metrics.body, /sump_request_duration_seconds_bucket\{le="\+Inf",method="GET"\}/);
  // 抓取本身算在飞请求，所以只断言它是数字（不假装为 0）。
  assert.match(metrics.body, /sump_inflight_requests \d+/);
});
