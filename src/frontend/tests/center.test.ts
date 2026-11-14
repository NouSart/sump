// CenterLink 行为测试：原生注册 / 名册与设置事件 / 心跳 / 调用 / 断线自愈。
// 控制面只剩 SPv2.1 /spv2 一条路：register 是 call，heartbeat 是 event，roster / settings 是事件推送。
import assert from 'node:assert/strict';
import test from 'node:test';
import { CenterLink, RemoteError } from '../src/server/center.ts';
import { SPv2UnavailableError } from '../src/server/spv2.ts';
import { StubNativeCenter } from './stub-native-center.ts';
import { waitFor } from './util.ts';

const silent = (): void => {};

function makeLink(url: string, heartbeatMS = 30): CenterLink {
  return new CenterLink({
    name: 'frontend',
    title: '门户',
    centerURL: url,
    selfURL: 'ws://127.0.0.1:1/spv2',
    description: '测试面板',
    provides: [],
    settings: [],
    heartbeatMS,
    log: silent,
  });
}

test('原生：注册 → roster/settings 事件 → 心跳 → 调用', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = new CenterLink({
    name: 'frontend',
    title: '门户',
    centerURL: url,
    selfURL: 'ws://127.0.0.1:1/spv2',
    description: '测试面板',
    provides: [{ action: 'message' }],
    settings: [{ key: 'center', default: 'ws://127.0.0.1:9000/spv2' }],
    heartbeatMS: 30,
    log: silent,
  });
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '原生注册');

  // register 是 SPv2 call，body 形状与名册注册载荷一致。
  assert.equal(stub.registrations.length, 1);
  const reg = stub.registrations[0] ?? {};
  assert.equal(reg.name, 'frontend');
  assert.equal(reg.title, '门户');
  assert.equal(reg.addr, 'ws://127.0.0.1:1/spv2');
  assert.equal(reg.description, '测试面板');
  assert.deepEqual(reg.provides, [{ action: 'message' }]);
  assert.deepEqual(reg.settings, [{ key: 'center', default: 'ws://127.0.0.1:9000/spv2' }]);
  assert.equal(reg.heartbeat_ms, 30);
  assert.equal(stub.calls[0]?.method, 'register');
  assert.equal(stub.calls[0]?.from, 'frontend');
  assert.deepEqual(link.roster, { services: [], revision: 1 });

  const events: string[] = [];
  const settings: unknown[] = [];
  link.on((e) => {
    events.push(e.kind);
    if (e.kind === 'settings') settings.push(e.event);
  });

  stub.broadcastRoster({
    services: [
      { name: 'svc-a', addr: 'ws://127.0.0.1:2/spv2' },
      { name: 'svc-b', addr: 'ws://127.0.0.1:3/spv2' },
    ],
    revision: 2,
  });
  await waitFor(() => link.roster?.revision === 2, 3000, '原生 roster 事件');
  assert.equal(link.roster?.services.length, 2);
  assert.ok(events.includes('roster'));

  stub.broadcastSettings({
    cause: 'register',
    service: 'svc-a',
    results: [{ key: 'tool.notify_mode', value: 'each', ok: true }],
  });
  await waitFor(() => settings.length === 1, 3000, '原生 settings 事件');
  const ev = settings[0] as { cause?: string; service?: string; results?: { key?: string; value?: string; ok?: boolean }[] };
  assert.equal(ev.cause, 'register');
  assert.equal(ev.service, 'svc-a');
  assert.equal(ev.results?.[0]?.key, 'tool.notify_mode');
  assert.equal(ev.results?.[0]?.value, 'each');
  assert.equal(ev.results?.[0]?.ok, true);

  // heartbeat 是 SPv2 event（不是 v1 信封）。
  await waitFor(() => stub.heartbeats >= 2, 3000, '原生心跳事件');
  const hb = stub.heartbeatBodies.at(-1) as { status?: string } | undefined;
  assert.equal(hb?.status, 'ok');

  // 控制面调用也走原生 call（不存在的 v1 jump 路径已删除）。
  stub.settingsReply = {
    services: [{ service: 'svc-a', settings: [{ key: 'k', value: 'v', default: 'd', overridden: false }] }],
  };
  const data = (await link.call('list_settings', {})) as { services: { service: string }[] };
  assert.equal(data.services[0]?.service, 'svc-a');
  assert.equal(stub.calls.at(-1)?.method, 'list_settings');
});

test('SPv2 原生断开后自动重连并重注册', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => stub.registrations.length === 1, 3000, '首次原生注册');
  await waitFor(() => link.connected, 2000, '连接建立');

  stub.closeConnections();
  await waitFor(() => !link.connected, 2000, '断开');
  await waitFor(() => stub.registrations.length === 2, 4000, '原生重连重注册');
  await waitFor(() => link.connected, 2000, '连接标记恢复');
});

test('SPv2 拨号失败：不回退 v1，按退避重试直到中心恢复', async (t) => {
  const stub = new StubNativeCenter();
  stub.down = true;
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => stub.attempts >= 1, 3000, '首次拨号');
  assert.equal(link.connected, false, '握手被拒时不能算已连接');
  assert.equal(stub.calls.length, 0, '连不上就没有注册（不存在 v1 回落路径）');

  // 中心恢复后，退避重试应自己接上并完成原生注册。
  stub.down = false;
  await waitFor(() => link.connected, 5000, '退避重试接上');
  assert.equal(stub.calls[0]?.method, 'register');
});

test('未连接 / 中心下线：调用立即失败', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });

  await assert.rejects(() => link.call('list_settings'), /未连接到设置中心/);

  link.start();
  await waitFor(() => link.connected, 3000, '连接建立');
  await stub.stop();
  await waitFor(() => !link.connected, 3000, '断开');
  await assert.rejects(() => link.call('list_settings'), /未连接到设置中心/);
});

test('注册一直失败：重连间隔逐步翻倍，不会每秒刷屏', async (t) => {
  const stub = new StubNativeCenter();
  stub.registerError = '中心还没准备好';
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => stub.registrations.length === 1, 3000, '首次注册尝试');
  await waitFor(() => stub.registrations.length === 2, 4000, '第一次重试');
  const firstGap = Date.now();
  await waitFor(() => stub.registrations.length === 3, 6000, '第二次重试');
  // 间隔按 1s → 2s 增长：第二次重试前的等待应明显长于 1s。
  assert.ok(Date.now() - firstGap >= 1600, `第二次重试间隔应已退避到 2s，实际 ${String(Date.now() - firstGap)}ms`);
});

test('jumpTo 走原生 dialSPv2Once', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '原生连接');
  stub.roster = { services: [{ name: 'target', addr: url }], revision: 2 };
  stub.broadcastRoster();
  await waitFor(() => link.roster?.services.length === 1, 3000, '目标名册');
  stub.settingsReply = { services: [{ service: 'target', settings: [] }] };
  const data = (await link.jumpTo('target', 'list_settings', {})) as { services: { service: string }[] };
  assert.equal(data.services[0]?.service, 'target');
  assert.equal(stub.calls.at(-1)?.method, 'list_settings');
  assert.equal(stub.calls.at(-1)?.from, 'frontend', '直连目标时 from 仍是门户自己（回答沿 from 回来）');
});

test('jumpTo 原生错误映射为 RemoteError', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '原生连接');
  stub.roster = { services: [{ name: 'target', addr: url }], revision: 2 };
  stub.broadcastRoster();
  await waitFor(() => link.roster?.services.length === 1, 3000, '目标名册');
  stub.setError = '目标拒绝了这个设置';
  await assert.rejects(
    () => link.jumpTo('target', 'set_setting', {}),
    (err: unknown) => err instanceof RemoteError && /目标拒绝/.test((err as Error).message),
  );
});

test('jumpTo 目标不可达：抛 SPv2UnavailableError，不再回落 v1', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '原生连接');
  // 端口 1 上没有任何服务：拨 /spv2 会失败，这正是过去触发 v1 回落的条件。
  stub.roster = { services: [{ name: 'target', addr: 'ws://127.0.0.1:1/spv2' }], revision: 2 };
  stub.broadcastRoster();
  await waitFor(() => link.roster?.services.length === 1, 3000, '目标名册');
  await assert.rejects(
    () => link.jumpTo('target', 'list_settings', {}),
    (err: unknown) => err instanceof SPv2UnavailableError,
    '不可达必须原样上报传输错误，不能悄悄换成 v1',
  );
});

test('jumpTo 缺目标名片时明确拒绝', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '连接');
  await assert.rejects(() => link.jumpTo('missing', 'x'), /未知目标服务/);
});

test('call 原生远端错误映射为 RemoteError（消息为远端错误）', async (t) => {
  const stub = new StubNativeCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '原生连接');
  stub.setError = '设置项未声明';
  await assert.rejects(
    () => link.call('set_setting', { service: 's', key: 'k', value: 'v' }),
    (err: unknown) => err instanceof RemoteError && /设置项未声明/.test((err as Error).message),
  );
});
