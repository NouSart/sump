// HTTP API 测试：overview / set / reset / SSE / 静态资源 / 错误路径（含中心不可达）。
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
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
    { name: 'frontend', addr: '127.0.0.1:0', centerURL, webDir: await makeWebDir(), heartbeatMS },
    silent,
  );
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
});
