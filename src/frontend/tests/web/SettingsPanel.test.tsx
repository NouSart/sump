// SettingsPanel（人话版）：空态 / 分组 / 状态词 / 开关与选项直接提交 / 服务名候选来自名册 /
// 数字单位与读法 / 长文本进「高级」/ 语言包覆盖（t 返回 x 时不出现中文）。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react';
import type { ComponentProps } from 'react';
import { SettingsPanel } from '../../src/web/components/SettingsPanel';
import type { Translate } from '../../src/web/i18n';
import type { RowStatus, SettingViewData } from '../../src/web/types';

// tZh 等价于“语言包缺失、一路走兜底中文”，正是默认路径。
const tZh: Translate = (key, fallback) => fallback ?? key;

const svc = { name: 'images', title: '图片', addr: 'ws://127.0.0.1:9401/ws', description: '图片服务' };

const defaultRow: SettingViewData = { key: 'max_mb', value: '32', default: '32', overridden: false, label: '单张图片大小上限', kind: 'number' };
const overriddenRow: SettingViewData = { key: 'owner', value: '10001', default: '', overridden: true };
const secretRow: SettingViewData = { key: 'llm.key', value: '', default: '', overridden: false, secret: true, set: true };
const secretUnsetRow: SettingViewData = { key: 'napcat.token', value: '', default: '', overridden: false, secret: true };
const readOnlyRow: SettingViewData = {
  key: 'addr',
  value: '127.0.0.1:9101',
  default: '127.0.0.1:9101',
  overridden: false,
  readonly: true,
  label: '监听地址',
};
const driftedRow: SettingViewData = {
  key: 'demo.mode',
  value: 'batch',
  default: 'each',
  overridden: true,
  actual: 'self-changed',
  reported: true,
};
const boolRow: SettingViewData = { key: 'flag', value: 'false', default: 'false', overridden: false, kind: 'bool' };
const enumRow: SettingViewData = {
  key: 'mode',
  value: 'each',
  default: 'each',
  overridden: false,
  kind: 'enum',
  options: ['each', 'all'],
};

function renderPanel(over: Partial<ComponentProps<typeof SettingsPanel>> = {}) {
  return render(
    <SettingsPanel
      service={svc}
      settings={[defaultRow, overriddenRow]}
      peers={['images', 'memory', 'reasoner']}
      connected={true}
      statusOf={() => undefined}
      onSave={async () => {}}
      onReset={async () => {}}
      t={tZh}
      {...over}
    />,
  );
}

describe('SettingsPanel', () => {
  it('没收到名册：一句人话，不摆键值', () => {
    renderPanel({ service: null, settings: null });
    assert.ok(screen.getByText('还没有收到任何服务的设置声明'));
  });

  it('标题用中文短名；能改的分一组，只读启动参数折叠起来', () => {
    renderPanel({ settings: [defaultRow, overriddenRow, readOnlyRow] });
    assert.ok(screen.getByRole('heading', { name: '图片' })); // 服务自报的 title
    assert.ok(screen.getByText('可以改的'));
    assert.ok(screen.getByText('启动参数'));
    // 只读项在 <details> 里（默认收起），但仍在文档里
    const ro = screen.getByText('监听地址');
    assert.ok(ro.closest('details'));
    assert.ok(screen.getByText('127.0.0.1:9101'));
  });

  it('状态说人话：默认值 / 已改过 / 待服务确认 / 未送达', () => {
    const statuses: Record<string, RowStatus> = {
      owner: { state: 'pending' },
      'demo.mode': { state: 'failed', error: '未送达: 服务离线' },
    };
    renderPanel({ settings: [defaultRow, overriddenRow, driftedRow], statusOf: (_s, key) => statuses[key] });
    assert.ok(screen.getByText('默认值'));
    assert.ok(screen.getByText('待服务确认'));
    const failed = screen.getByText('未送达');
    assert.equal(failed.getAttribute('title'), '未送达: 服务离线');
    assert.ok(screen.getByText('服务侧已变'));
  });

  it('秘密项：只显示有没有值，覆盖走密码框，值原样交给 onSave', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ settings: [secretRow], onSave });
    assert.ok(screen.getByText('已设置'));
    assert.ok(screen.getByText('••••••••'));
    assert.ok(screen.queryByText('默认值') === null);

    fireEvent.click(screen.getByRole('button', { name: '修改' }));
    const input = screen.getByLabelText('llm.key 修改') as HTMLInputElement;
    assert.equal(input.type, 'password');
    assert.equal(input.value, '');
    assert.equal((screen.getByRole('button', { name: '保存' }) as HTMLButtonElement).disabled, true);
    fireEvent.change(input, { target: { value: 'sk-2f9c' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'llm.key', 'sk-2f9c'));
  });

  it('秘密项未设置：明确写"还没设置"', () => {
    renderPanel({ settings: [secretUnsetRow] });
    assert.ok(screen.getByText('还没设置'));
  });

  it('秘密项已设置：清空要两步确认，确认后走 onReset', async () => {
    const onReset = vi.fn(async () => {});
    renderPanel({ settings: [secretRow], onReset });

    fireEvent.click(screen.getByRole('button', { name: '清空' }));
    expect(onReset).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole('button', { name: '再点一次确认清空' }));
    await waitFor(() => expect(onReset).toHaveBeenCalledWith('images', 'llm.key'));
  });

  it('秘密项未设置：没有清空入口', () => {
    renderPanel({ settings: [secretUnsetRow] });
    assert.equal(screen.queryByRole('button', { name: '清空' }), null);
  });

  it('编辑保存：调用 onSave(service,key,新值)，成功后退出编辑态', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByRole('button', { name: '修改' })[0]!);
    const input = screen.getByLabelText('单张图片大小上限 修改') as HTMLInputElement;
    assert.equal(input.value, '32');
    fireEvent.change(input, { target: { value: '64' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'max_mb', '64'));
    await waitFor(() => assert.equal(screen.queryByLabelText('单张图片大小上限 修改'), null));
  });

  it('值未改动：保存直接退出，不调用 onSave', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByRole('button', { name: '修改' })[0]!);
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => assert.equal(screen.queryByLabelText('单张图片大小上限 修改'), null));
    expect(onSave).not.toHaveBeenCalled();
  });

  it('Escape 取消编辑，不调用 onSave', () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByRole('button', { name: '修改' })[0]!);
    const input = screen.getByLabelText('单张图片大小上限 修改');
    fireEvent.keyDown(input, { key: 'Escape' });
    assert.equal(screen.queryByLabelText('单张图片大小上限 修改'), null);
    expect(onSave).not.toHaveBeenCalled();
  });

  it('保存失败：保持编辑态（让用户可修正）', async () => {
    const onSave = vi.fn(async () => {
      throw new Error('设置项未声明');
    });
    renderPanel({ onSave });
    fireEvent.click(screen.getAllByRole('button', { name: '修改' })[0]!);
    fireEvent.change(screen.getByLabelText('单张图片大小上限 修改'), { target: { value: '999' } });
    fireEvent.click(screen.getByRole('button', { name: '保存' }));
    await waitFor(() => expect(onSave).toHaveBeenCalled());
    assert.ok(screen.getByLabelText('单张图片大小上限 修改'));
  });

  it('布尔项：一个开关，点一下就是一次提交', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ settings: [boolRow], onSave });
    const sw = screen.getByRole('switch', { name: 'flag' });
    assert.equal(sw.getAttribute('aria-checked'), 'false');
    fireEvent.click(sw);
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'flag', 'true'));
  });

  it('枚举项：候选项直接摆出来，点一下就是一次提交（不用先进编辑态）', async () => {
    const onSave = vi.fn(async () => {});
    renderPanel({ settings: [enumRow], onSave });
    const radios = screen.getAllByRole('radio');
    assert.deepEqual(
      radios.map((r) => r.textContent),
      ['each', 'all'],
    );
    assert.equal(radios[0]?.getAttribute('aria-checked'), 'true');
    fireEvent.click(radios[1] as HTMLElement);
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'mode', 'all'));
  });

  it('枚举项候选多于 3 个时改下拉，仍是点一下就提交', async () => {
    const onSave = vi.fn(async () => {});
    const many: SettingViewData = { ...enumRow, value: 'a', kind: 'enum', options: ['a', 'b', 'c', 'd'] };
    renderPanel({ settings: [many], onSave });
    const select = screen.getByLabelText('mode') as HTMLSelectElement;
    assert.deepEqual(
      [...select.options].map((o) => o.value),
      ['a', 'b', 'c', 'd'],
    );
    fireEvent.change(select, { target: { value: 'd' } });
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'mode', 'd'));
  });

  it('服务名项：候选来自名册（排除自己），选完即提交', async () => {
    const onSave = vi.fn(async () => {});
    const svcRow: SettingViewData = {
      key: 'reasoner',
      value: 'reasoner',
      default: 'reasoner',
      overridden: false,
      kind: 'service',
    };
    renderPanel({ settings: [svcRow], onSave });
    const select = screen.getByLabelText('reasoner') as HTMLSelectElement;
    assert.deepEqual(
      [...select.options].map((o) => o.value),
      ['memory', 'reasoner'],
    );
    fireEvent.change(select, { target: { value: 'memory' } });
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'reasoner', 'memory'));
  });

  it('可空服务项：一个开关 + 选择；关掉提交空串，打开回落默认值', async () => {
    const onSave = vi.fn(async () => {});
    const optRow: SettingViewData = {
      key: 'portal',
      value: 'frontend',
      default: 'frontend',
      overridden: false,
      kind: 'optional-service',
    };
    const { rerender } = renderPanel({ settings: [optRow], onSave });
    const sw = screen.getByRole('switch', { name: 'portal' });
    assert.equal(sw.getAttribute('aria-checked'), 'true');
    fireEvent.click(sw);
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'portal', ''));

    rerender(
      <SettingsPanel
        service={svc}
        settings={[{ ...optRow, value: '' }]}
        peers={['images', 'memory', 'reasoner']}
        connected={true}
        statusOf={() => undefined}
        onSave={onSave}
        onReset={async () => {}}
        t={tZh}
      />,
    );
    const swOff = screen.getByRole('switch', { name: 'portal' });
    assert.equal(swOff.getAttribute('aria-checked'), 'false');
    fireEvent.click(swOff);
    await waitFor(() => expect(onSave).toHaveBeenCalledWith('images', 'portal', 'frontend'));
  });

  it('数字项：带单位与读法（300000 毫秒 ≈ 5 分钟）', () => {
    const msRow: SettingViewData = {
      key: 'timeout',
      value: '300000',
      default: '300000',
      overridden: false,
      kind: 'number',
      unit: 'ms',
    };
    renderPanel({ settings: [msRow] });
    assert.ok(screen.getByText('毫秒'));
    assert.ok(screen.getByText('≈ 5 分钟'));
  });

  it('长文本项收进「高级」，正文里只剩开关与选项', () => {
    const areaRow: SettingViewData = {
      key: 'policy.json',
      value: '{}',
      default: '{}',
      overridden: false,
      kind: 'textarea',
    };
    renderPanel({ settings: [areaRow] });
    assert.ok(screen.getByText('高级'));
    assert.ok(screen.getByText('这个服务没有开关式的设置，只有下面这些高级项'));
  });

  it('重置：两步确认（先确认后执行），确认窗口 3s 超时复位', async () => {
    vi.useFakeTimers();
    try {
      const onReset = vi.fn(async () => {});
      renderPanel({ onReset });
      fireEvent.click(screen.getByRole('button', { name: '恢复默认' }));
      assert.ok(screen.getByRole('button', { name: '再点一次确认恢复默认' }));
      expect(onReset).not.toHaveBeenCalled();

      await act(async () => {
        await vi.advanceTimersByTimeAsync(3100);
      });
      assert.ok(screen.getByRole('button', { name: '恢复默认' }));

      fireEvent.click(screen.getByRole('button', { name: '恢复默认' }));
      fireEvent.click(screen.getByRole('button', { name: '再点一次确认恢复默认' }));
      await act(async () => {
        await vi.advanceTimersByTimeAsync(0);
      });
      expect(onReset).toHaveBeenCalledWith('images', 'owner');
    } finally {
      vi.useRealTimers();
    }
  });

  it('未连接：修改与恢复默认都禁用', () => {
    renderPanel({ connected: false });
    const edit = screen.getAllByRole('button', { name: '修改' })[0] as HTMLButtonElement;
    const reset = screen.getByRole('button', { name: '恢复默认' }) as HTMLButtonElement;
    assert.equal(edit.disabled, true);
    assert.equal(reset.disabled, true);
  });

  it('语言包覆盖：t 给什么就显示什么（全 x 时界面不该再露中文）', () => {
    const tX: Translate = (key, fallback) => (key.startsWith('setting.') || key.startsWith('ui.') ? 'x' : (fallback ?? key));
    renderPanel({ settings: [defaultRow], t: tX });
    assert.ok(screen.getAllByText('x').length > 0);
    assert.equal(screen.queryByText('默认值'), null);
    assert.equal(screen.queryByText('单张图片大小上限'), null);
  });
});