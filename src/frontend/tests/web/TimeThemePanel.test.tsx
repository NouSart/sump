// TimeThemePanel：外观设置只做受控展示——开关/滑块把用户意图交给 timeTheme，
// 自己不留状态；跟随与固定两种文案必须说清楚当前处于哪种。
import assert from 'node:assert/strict';
import { describe, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { TimeThemePanel } from '../../src/web/components/TimeThemePanel';
import type { Translate } from '../../src/web/i18n';
import type { TimeThemeControls } from '../../src/web/timeTheme';

const t: Translate = (key, fallback) => fallback ?? key;

function controls(over: Partial<TimeThemeControls> = {}): TimeThemeControls {
  return {
    auto: true,
    hour: 9.5,
    label: '上午',
    setAuto: vi.fn(),
    setHour: vi.fn(),
    ...over,
  };
}

describe('TimeThemePanel', () => {
  it('跟随中：开关打开，点击交给 setAuto(false)', () => {
    const c = controls();
    render(<TimeThemePanel timeTheme={c} t={t} />);

    // 标签颜色与开关状态是两条独立事实：只查“文字存在”会自证。
    assert.ok(screen.getByText('跟随中').className.includes('tag-default'));
    const sw = screen.getByRole('switch');
    assert.equal(sw.getAttribute('aria-checked'), 'true');
    fireEvent.click(sw);
    assert.deepEqual((c.setAuto as ReturnType<typeof vi.fn>).mock.calls, [[false]]);
  });

  it('已固定：开关关闭，点击交回 setAuto(true)', () => {
    const c = controls({ auto: false, label: '深夜' });
    render(<TimeThemePanel timeTheme={c} t={t} />);

    assert.ok(screen.getByText('已固定').className.includes('tag-over'));
    assert.match(screen.getByText(/已固定为/).textContent ?? '', /深夜/);
    fireEvent.click(screen.getByRole('switch'));
    assert.deepEqual((c.setAuto as ReturnType<typeof vi.fn>).mock.calls, [[true]]);
  });

  it('拖动滑块：把数值原样交给 setHour，读数按 HH:MM 展示', () => {
    const c = controls();
    render(<TimeThemePanel timeTheme={c} t={t} />);

    const slider = screen.getByRole('slider');
    assert.equal(slider.getAttribute('aria-valuetext'), '09:30 · 上午');
    fireEvent.change(slider, { target: { value: '14.25' } });
    assert.deepEqual((c.setHour as ReturnType<typeof vi.fn>).mock.calls, [[14.25]]);
  });
});
