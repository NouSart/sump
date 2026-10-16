// App 集成测试（逻辑层）：首屏加载 / 服务切换 / 写入→待确认→applied 事件（toast+状态回落）/
// 失败事件（未送达+原因）/ 写入请求失败（编辑态保留）/ roster revision 触发重拉 /
// status 事件 / toast 自动消失（假时钟）/ 外观开关（属性 + 本地记忆）。
//
// 说明：SSE 与 fetch 都在边界处替换（假 EventSource / fetch 路由桩），App 代码零改动。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react';
import { App } from '../../src/web/App';
import { remainingText } from '../../src/web/components/PortalView';
import type { RosterView } from '../../src/web/types';
import {
  countCalls,
  installFakeEventSource,
  jsonResponse,
  latestSource,
  overviewOf,
  rosterOf,
  settingsOf,
  stubFetch,
} from './harness';

const okOverview = (r: RosterView) =>
  overviewOf(r, [settingsOf('images', [{ key: 'max_mb', value: '1', default: '32', overridden: true }])]);

// boot 安装替身并挂载 App：
//   - setResponse 可替换 set_setting 的应答（默认成功）；
//   - messageResponse 可替换 /api/message 的应答（默认受理）；
//   - openSettings=false 停在门户视图（设置现在要从侧栏底部进入）；
//   - 返回的 setRoster 用于让 overview 桩跟随名册推进（与服务端一致：名册更新后重拉应见到新名册）。
function boot(over: { setResponse?: () => Response; messageResponse?: () => Response; openSettings?: boolean } = {}) {
  let currentRoster = rosterOf(['images', 'qq']);
  const calls = stubFetch((url) => {
    if (url === '/api/overview') return jsonResponse(okOverview(currentRoster));
    if (url === '/api/message') {
      if (over.messageResponse) return over.messageResponse();
      return jsonResponse({ ok: true, conversationId: 'web:private:owner' });
    }
    if (url === '/api/settings/set') {
      if (over.setResponse) return over.setResponse();
      return jsonResponse({
        ok: true,
        services: [settingsOf('images', [{ key: 'max_mb', value: '2', default: '32', overridden: true }])],
      });
    }
    if (url === '/api/settings/reset') {
      return jsonResponse({
        ok: true,
        services: [settingsOf('images', [{ key: 'max_mb', value: '32', default: '32', overridden: false }])],
      });
    }
    return undefined;
  });
  installFakeEventSource();
  render(<App />);
  if (over.openSettings !== false) {
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
  }
  return {
    calls,
    setRoster: (r: RosterView) => {
      currentRoster = r;
    },
  };
}

// writeNewValue 在 images 的第一行进入编辑、改值并保存。
function writeNewValue(value: string): void {
  fireEvent.click(screen.getAllByText('修改')[0]!);
  fireEvent.change(screen.getByLabelText('max_mb 的新值'), { target: { value } });
  fireEvent.click(screen.getByText('保存'));
}

// emitSSE 在 act 中触发一个 SSE 事件。
async function emitSSE(type: string, data: unknown): Promise<void> {
  await act(async () => {
    latestSource().emit(type, data);
  });
}

describe('App', () => {
  it('首屏：名册/状态/角标；切换服务联动面板', async () => {
    boot();
    await screen.findByRole('heading', { name: 'images' }); // 默认选中第一个服务
    assert.ok(screen.getByText('服务 · 2'));
    assert.ok(screen.getByText('已连接'));
    assert.ok(screen.getByTitle('1 个设置项')); // images 有 1 个设置项 → 角标

    fireEvent.click(screen.getByText('qq'));
    assert.ok(screen.getByRole('heading', { name: 'qq' }));
    assert.ok(screen.getByText(/该服务未声明可设置项/));

    fireEvent.click(screen.getByText('images'));
    assert.ok(screen.getByRole('heading', { name: 'images' }));
    assert.ok(screen.getByText('max_mb'));
  });

  it('写入：受理后待确认 → applied 事件 → 已生效 toast + 状态回落 + 重拉视图', async () => {
    const { calls } = boot();
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('2');
    await screen.findByText('已写入 images.max_mb（待服务确认）');
    assert.ok(screen.getByText('已覆盖 · 待确认'));

    await emitSSE('settings', {
      cause: 'set_setting',
      service: 'images',
      results: [{ key: 'max_mb', value: '2', ok: true }],
    });
    await screen.findByText('已生效 images.max_mb = 2');
    await waitFor(() => assert.ok(screen.getByText('已覆盖'))); // 待确认清除，覆盖态保留
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2)); // 事件触发重拉
    expect(calls.some((c) => c.url === '/api/settings/set' && c.init?.method === 'POST')).toBe(true);
  });

  it('服务自报事件（report_setting）不冒充推送回执：待确认状态不被动，视图照旧重拉', async () => {
    const { calls } = boot();
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('2');
    await screen.findByText('已写入 images.max_mb（待服务确认）');

    await emitSSE('settings', {
      cause: 'report_setting',
      service: 'images',
      results: [{ key: 'max_mb', ok: true }],
    });
    // 不 toast、不清状态：那一条不是我们这次推送的回执
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2));
    assert.ok(screen.getByText('已覆盖 · 待确认'));
    assert.equal(screen.queryByText(/已生效 images\.max_mb/), null);
  });

  it('失败事件：未送达 + 原因（行状态与 toast）', async () => {
    boot();
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('2');
    await screen.findByText('已写入 images.max_mb（待服务确认）');
    await emitSSE('settings', {
      cause: 'set_setting',
      service: 'images',
      results: [{ key: 'max_mb', ok: false, error: '未送达: 拨号失败' }],
    });

    await screen.findByText('未生效 images.max_mb：未送达: 拨号失败');
    const failed = screen.getByText('已覆盖 · 未送达');
    assert.equal(failed.getAttribute('title'), '未送达: 拨号失败');
  });

  it('写入请求被中心拒绝：写入失败 toast，编辑态保留', async () => {
    boot({ setResponse: () => jsonResponse({ error: '设置项未声明' }, 400) });
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('9');
    await screen.findByText('写入失败：设置项未声明');
    assert.ok(screen.getByLabelText('max_mb 的新值')); // 仍在编辑，用户可修正
    assert.ok(screen.getByText('已覆盖')); // pending 已清除
  });

  it('roster revision 变化：名册更新并触发设置视图重拉', async () => {
    const { calls, setRoster } = boot();
    await screen.findByRole('heading', { name: 'images' });

    const r2 = rosterOf(['images', 'qq', 'memory'], 2);
    setRoster(r2); // 服务端侧名册已推进（重拉 overview 应见到新名册）
    await emitSSE('roster', r2);
    await screen.findByText('服务 · 3');
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2));
  });

  it('status 事件：连接状态切换；重连（false→true）触发重拉', async () => {
    const { calls } = boot();
    await screen.findByRole('heading', { name: 'images' });

    await emitSSE('status', { connected: false });
    await screen.findByText('未连接');

    await emitSSE('status', { connected: true });
    await screen.findByText('已连接');
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2));
  });

  it('toast 4200ms 后自动消失（假时钟；不用 waitFor——其内部靠真实 interval 检测假时钟）', async () => {
    vi.useFakeTimers();
    try {
      boot({ setResponse: () => jsonResponse({ error: '设置项未声明' }, 400) });
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10); // 冲掉首屏加载的微任务
      });
      assert.ok(screen.getByRole('heading', { name: 'images' }));

      writeNewValue('9');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10); // 冲掉保存失败与 toast 入列
      });
      assert.ok(screen.getByText('写入失败：设置项未声明'));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4300); // 超过 4200ms 自动消失窗口
      });
      assert.equal(screen.queryByText('写入失败：设置项未声明'), null);
    } finally {
      vi.useRealTimers();
    }
  });

  it('门户：首屏在门户视图，设置从侧栏底部进入并可关闭', async () => {
    boot({ openSettings: false });
    await screen.findByRole('heading', { name: /门户/ });
    assert.equal(screen.queryByRole('heading', { name: '设置' }), null, '首屏不该直接是设置页');

    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    assert.ok(screen.getByRole('heading', { name: '设置' }));
    assert.ok(screen.getByText('服务 · 2'), '设置页里应有服务导航');

    fireEvent.click(screen.getByRole('button', { name: '关闭' }));
    assert.ok(screen.getByRole('heading', { name: /门户/ }));
  });

  it('门户：deliver 事件把智能体的消息落进对话', async () => {
    boot({ openSettings: false });
    await screen.findByRole('heading', { name: /门户/ });

    await emitSSE('deliver', { text: '任务完成', conversationId: 'web:private:owner', at: '2026-10-16T08:00:00Z' });
    await screen.findByText('任务完成');
  });

  it('门户：发送 → POST /api/message + 本地回显', async () => {
    const { calls } = boot({ openSettings: false });
    await screen.findByRole('heading', { name: /门户/ });

    fireEvent.change(screen.getByLabelText('消息'), { target: { value: '你好' } });
    fireEvent.click(screen.getByText('发送'));

    await screen.findByText('你好'); // 本地回显（受理回执不代表已有回复）
    await waitFor(() => expect(calls.some((c) => c.url === '/api/message' && c.init?.method === 'POST')).toBe(true));
    assert.equal(screen.queryByRole('alert'), null, '受理成功不应留下错误提示');
  });

  it('门户：发送未被受理 → 明确提示未送达', async () => {
    boot({ openSettings: false, messageResponse: () => jsonResponse({ error: '未知目标服务: reasoner（不在名册里）' }, 503) });
    await screen.findByRole('heading', { name: /门户/ });

    fireEvent.change(screen.getByLabelText('消息'), { target: { value: '你好' } });
    fireEvent.click(screen.getByText('发送'));

    await screen.findByText(/未送达：未知目标服务: reasoner/);
  });

  it('外观：切深色写 body[data-ds-dark-theme] 并记住，切回浅色移除', () => {
    window.localStorage.clear();
    delete document.body.dataset.dsDarkTheme;
    boot({ openSettings: false });

    assert.equal(document.body.hasAttribute('data-ds-dark-theme'), false, '默认浅色不带属性');
    assert.equal(screen.getByRole('radio', { name: '浅色' }).getAttribute('aria-checked'), 'true');

    fireEvent.click(screen.getByRole('radio', { name: '深色' }));
    assert.equal(document.body.getAttribute('data-ds-dark-theme'), '', '暗色靠这个属性切令牌');
    assert.equal(screen.getByRole('radio', { name: '深色' }).getAttribute('aria-checked'), 'true');
    assert.equal(window.localStorage.getItem('sump.portal.theme'), 'dark');

    fireEvent.click(screen.getByRole('radio', { name: '浅色' }));
    assert.equal(document.body.hasAttribute('data-ds-dark-theme'), false);
    assert.equal(window.localStorage.getItem('sump.portal.theme'), 'light');
  });

  it('门户：timer 快照画成倒计时；到点显示「到点…」，空快照即消失', async () => {
    boot({ openSettings: false });
    await screen.findByRole('heading', { name: /门户/ });

    const soon = new Date(Date.now() + 90_000).toISOString();
    await emitSSE('timers', [{ itemId: 'job-1', caller: 'reasoner', ms: 90000, fireAt: soon, note: '等构建' }]);
    await screen.findByText(/\d{2}:\d{2} 后唤醒/);
    assert.ok(screen.getByText('等构建'), '备注要显示出来');

    // 到点（fire_at 已过去）但快照还没更新 → 显示「到点…」，不假装没有计时。
    await emitSSE('timers', [
      { itemId: 'job-1', caller: 'reasoner', ms: 1000, fireAt: new Date(Date.now() - 1000).toISOString(), note: '' },
    ]);
    await screen.findByText('到点…');

    // timer 推来不含它的快照 → 倒计时消失。
    await emitSSE('timers', []);
    await waitFor(() => assert.equal(screen.queryByText('到点…'), null));
  });

  it('倒计时读数格式：不到一小时 mm:ss，超过带小时，已过归零', () => {
    assert.equal(remainingText(90_000), '01:30');
    assert.equal(remainingText(3_600_000), '1:00:00');
    assert.equal(remainingText(-5), '00:00');
  });

  it('外观：本地存过深色时挂载即生效', () => {
    window.localStorage.setItem('sump.portal.theme', 'dark');
    delete document.body.dataset.dsDarkTheme;
    boot({ openSettings: false });

    assert.equal(document.body.getAttribute('data-ds-dark-theme'), '');
    assert.equal(screen.getByRole('radio', { name: '深色' }).getAttribute('aria-checked'), 'true');

    window.localStorage.clear();
    delete document.body.dataset.dsDarkTheme;
  });
});
