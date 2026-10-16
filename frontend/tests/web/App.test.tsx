// App 集成测试（逻辑层）：首屏加载 / 服务切换 / 写入→待确认→applied 事件（toast+状态回落）/
// 失败事件（未送达+原因）/ 写入请求失败（编辑态保留）/ roster revision 触发重拉 /
// status 事件 / toast 自动消失（假时钟）。
//
// 说明：SSE 与 fetch 都在边界处替换（假 EventSource / fetch 路由桩），App 代码零改动。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react';
import { App } from '../../src/web/App';
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
//   - 返回的 setRoster 用于让 overview 桩跟随名册推进（与服务端一致：名册更新后重拉应见到新名册）。
function boot(over: { setResponse?: () => Response } = {}) {
  let currentRoster = rosterOf(['images', 'qq']);
  const calls = stubFetch((url) => {
    if (url === '/api/overview') return jsonResponse(okOverview(currentRoster));
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

  it('名册竞态：SSE 新名册先到、首屏 overview 的旧响应后到 → 旧名册不得覆盖，守卫水位也不得倒退', async () => {
    // 场景：首屏 fetch 被"网络延迟"扣住（gate），期间 SSE 推来 rev 2 的新名册；
    // 随后 fetch 才带着 rev 1 的旧名册返回。旧代码会无条件 setRoster → 界面回退到两个服务，
    // 并把修订水位倒退回 1（于是下一条 rev 2 事件会再触发一次多余重拉）。
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const calls = stubFetch((url) => {
      if (url === '/api/overview') {
        return gate.then(() => jsonResponse(okOverview(rosterOf(['images', 'qq'], 1))));
      }
      return undefined;
    });
    installFakeEventSource();
    render(<App />);

    // SSE 先到：rev 2，三个服务。
    await emitSSE('roster', rosterOf(['images', 'qq', 'memory'], 2));
    await screen.findByText('服务 · 3');

    // 旧响应此时才回来（rev 1，两个服务）——必须被丢弃。
    await act(async () => {
      release();
      await Promise.resolve();
    });
    assert.ok(screen.getByText('服务 · 3'), '更旧 revision 的名册不得覆盖新名册');

    // 守卫水位未倒退：再次收到 rev 2（与当前相同）→ 不触发多余重拉。
    await emitSSE('roster', rosterOf(['images', 'qq', 'memory'], 2));
    assert.equal(countCalls(calls, '/api/overview'), 2, '相同 revision 不应触发重拉');
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
});
