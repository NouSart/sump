// CenterLink 行为测试：注册字段 / 名册广播 / 心跳 / 调用 / 断线自愈。
import assert from 'node:assert/strict';
import test from 'node:test';
import { CenterLink } from '../src/server/center.ts';
import { StubCenter } from './stub-center.ts';
import { waitFor } from './util.ts';

const silent = (): void => {};

function makeLink(url: string, heartbeatMS = 30): CenterLink {
  return new CenterLink({
    name: 'frontend',
    centerURL: url,
    selfURL: 'ws://127.0.0.1:1/ws',
    description: '测试面板',
    provides: [],
    settings: [],
    heartbeatMS,
    log: silent,
  });
}

test('注册 → 名册 → 心跳 → 调用', async (t) => {
  const stub = new StubCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();

  await waitFor(() => link.connected, 3000, '连接建立');
  assert.equal(stub.registrations.length, 1);
  const reg = stub.registrations[0] ?? {};
  assert.equal(reg.name, 'frontend');
  assert.equal(reg.addr, 'ws://127.0.0.1:1/ws');
  assert.equal(reg.description, '测试面板');
  assert.equal(reg.heartbeat_ms, 30);

  // 注册响应携带名册（初始为空）
  assert.deepEqual(link.roster, { services: [], revision: 1 });

  // 广播名册 → 更新 + 事件
  const events: string[] = [];
  link.on((e) => events.push(e.kind));
  stub.broadcastRoster({
    services: [
      { name: 'svc-a', addr: 'ws://127.0.0.1:2/ws' },
      { name: 'svc-b', addr: 'ws://127.0.0.1:3/ws' },
    ],
    revision: 2,
  });
  await waitFor(() => link.roster?.revision === 2, 3000, '名册广播');
  assert.equal(link.roster?.services.length, 2);
  assert.ok(events.includes('roster'));

  // 心跳：间隔 30ms，等 ≥2 次
  await waitFor(() => stub.heartbeats >= 2, 3000, '心跳');

  // 调用：jump → response.data 二次解码
  stub.settingsReply = {
    services: [{ service: 'svc-a', settings: [{ key: 'k', value: 'v', default: 'd', overridden: false }] }],
  };
  const data = (await link.call('list_settings', {})) as { services: { service: string }[] };
  assert.equal(data.services[0]?.service, 'svc-a');
  const lastJump = stub.jumps.at(-1);
  assert.equal(lastJump?.action, 'list_settings');
  assert.equal(lastJump?.from, 'frontend');
});

test('未连接 / 中心下线：调用立即失败', async (t) => {
  const stub = new StubCenter();
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

test('断开后自动重连并重注册', async (t) => {
  const stub = new StubCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => stub.registrations.length === 1, 3000, '首次注册');

  stub.closeConnections();
  await waitFor(() => !link.connected, 2000, '断开');
  await waitFor(() => stub.registrations.length === 2, 4000, '重连重注册');
  assert.equal(link.connected, true);
});

test('中心明确回错误：调用抛 Error（消息为远端错误）', async (t) => {
  const stub = new StubCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '连接建立');

  stub.setError = '设置项未声明';
  await assert.rejects(
    () => link.call('set_setting', { service: 's', key: 'k', value: 'v' }),
    /设置项未声明/,
  );
});

test('settings 事件：中心广播的发放结果转成事件', async (t) => {
  const stub = new StubCenter();
  const url = await stub.start();
  const link = makeLink(url);
  t.after(async () => {
    link.stop();
    await stub.stop();
  });
  link.start();
  await waitFor(() => link.connected, 3000, '连接建立');

  const seen: unknown[] = [];
  link.on((e) => {
    if (e.kind === 'settings') seen.push(e.event);
  });
  stub.broadcastSettings({
    cause: 'register',
    service: 'svc-a',
    results: [{ key: 'tool.notify_mode', value: 'each', ok: true }],
  });
  await waitFor(() => seen.length === 1, 3000, 'settings 事件');
  const ev = seen[0] as { cause?: string; service?: string; results?: { key?: string; value?: string; ok?: boolean }[] };
  assert.equal(ev.cause, 'register');
  assert.equal(ev.service, 'svc-a');
  assert.equal(ev.results?.[0]?.key, 'tool.notify_mode');
  assert.equal(ev.results?.[0]?.value, 'each');
  assert.equal(ev.results?.[0]?.ok, true);
});
