// SettingsPanel：空态 / 表格五列 / 行状态（默认·已覆盖·待确认·未送达）/
// 编辑保存 / 值未变 / Escape 取消 / 重置两步确认与超时复位 / 未连接禁用。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react';
import type { ComponentProps } from 'react';
import { SettingsPanel } from '../../src/web/components/SettingsPanel';
import type { RowStatus, SettingViewData } from '../../src/web/types';

const svc = { name: 'images', addr: 'ws://127.0.0.1:9401/ws', description: '图片服务' };

const defaultRow: SettingViewData = { key: 'max_mb', value: '32', default: '32', overridden: false };
const overriddenRow: SettingViewData = { key: 'owner', value: '10001', default: '', overridden: true };

function renderPanel(over: Partial<ComponentProps<typeof SettingsPanel>> = {}) {
  return render(
    <SettingsPanel
      service={svc}
      settings={[defaultRow, overriddenRow]}
      connected={true}
      index={1}
      total={2}
      statusOf={() => undefined}
      onSave={async () => {}}
      onReset={async () => {}}
      {...over}
    />,
  );
}

describe('SettingsPanel', () => {
  it('无选中服务：等待名册提示', () => {
    renderPanel({ service: null });
    assert.ok(screen.getByText(/尚未收到名册/));
  });

  it('设置加载中（null）与未声明（空数组）两种空态', () => {
    const { unmount } = renderPanel({ settings: null });
    assert.ok(screen.getByText('加载中…'));
    unmount();
    renderPanel({ settings: [] });
    assert.ok(screen.getByText(/该服务未声明可设置项/));
  });

  it('表格五列 + 行内容：值 / 默认值（空回落 —）/ 状态标签', () => {
    renderPanel();
    for (const head of ['键', '生效值', '默认值', '状态', '操作']) {
      assert.ok(screen.getByText(head), `缺表头: ${head}`);
    }
    assert.ok(screen.getByText('max_mb'));
    assert.ok(screen.getAllByText('32').length >= 1);
    assert.ok(screen.getByText('默认'));
    // 覆盖行：无默认值显示 —；状态为已覆盖；重置按钮只在覆盖行出现
    assert.ok(screen.getByText('—'));
    assert.ok(screen.getByText('已覆盖'));
    assert.equal(screen.getAllByText('重置').length, 1);
  });

  it('行状态：待确认 / 未送达（带原因 title）优先于覆盖态', () => {
    const statuses: Record<string, RowStatus> = {
      max_mb: { state: 'pending' },
      owner: { state: 'failed', error: '未送达: 服务离线' },
    };
    renderPanel({ statusOf: (_s, key) => statuses[key] });
    assert.ok(screen.getByText('已覆盖 · 待确认'));
    const failed = screen.getByText('已覆盖 · 未送达');
    assert.equal(failed.getAttribute('title'), '未送达: 服务离线');
  });

  it('未连接：修改与重置按钮禁用', () => {
    renderPanel({ connected: false });
    const modify = screen.getAllByText('修改')[0]!.closest('button');
    const reset = screen.getByText('重置').closest('button');
    assert.equal(modify?.disabled, true);
    assert.equal(reset?.disabled, true);
  });

  it('编辑保存：调用 onSave(service,key,新值)，成功后退出编辑态', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByText('修改')[0]!);
    const input = screen.getByLabelText('max_mb 的新值') as HTMLInputElement;
    assert.equal(input.value, '32');
    fireEvent.change(input, { target: { value: '64' } });
    fireEvent.click(screen.getByText('保存'));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'max_mb', '64'));
    await waitFor(() => assert.equal(screen.queryByLabelText('max_mb 的新值'), null));
  });

  it('值未改动：保存直接退出，不调用 onSave', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByText('修改')[0]!);
    fireEvent.click(screen.getByText('保存'));
    await waitFor(() => assert.equal(screen.queryByLabelText('max_mb 的新值'), null));
    expect(onSave).not.toHaveBeenCalled();
  });

  it('Escape 取消编辑，不调用 onSave', () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByText('修改')[0]!);
    const input = screen.getByLabelText('max_mb 的新值');
    fireEvent.keyDown(input, { key: 'Escape' });
    assert.equal(screen.queryByLabelText('max_mb 的新值'), null);
    expect(onSave).not.toHaveBeenCalled();
  });

  it('保存失败：保持编辑态（让用户可修正），由上层 toast 提示', async () => {
    const onSave = vi.fn(async () => {
      throw new Error('设置项未声明');
    });
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByText('修改')[1]!);
    fireEvent.change(screen.getByLabelText('owner 的新值'), { target: { value: '20002' } });
    fireEvent.click(screen.getByText('保存'));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'owner', '20002'));
    // 仍在编辑态
    assert.ok(screen.getByLabelText('owner 的新值'));
  });

  it('重置：两步确认（先确认后执行），确认窗口 3s 超时复位', async () => {
    vi.useFakeTimers();
    try {
      const onReset = vi.fn(async () => {});
      renderPanel({ onReset });
      fireEvent.click(screen.getByText('重置'));
      assert.ok(screen.getByText('确认重置'));
      expect(onReset).not.toHaveBeenCalled();

      // 3s 未确认 → 复位
      await act(async () => {
        await vi.advanceTimersByTimeAsync(3100);
      });
      assert.ok(screen.getByText('重置'));
      assert.equal(screen.queryByText('确认重置'), null);

      // 再走一遍完整确认（假时钟下不用 waitFor——测试库的 waitFor 靠真实 interval 检测，会挂住）
      fireEvent.click(screen.getByText('重置'));
      fireEvent.click(screen.getByText('确认重置'));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(onReset).toHaveBeenCalledWith('images', 'owner');
      assert.ok(screen.getByText('重置')); // 确认态退出
    } finally {
      vi.useRealTimers();
    }
  });
});
