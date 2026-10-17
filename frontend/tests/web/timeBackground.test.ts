import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import {
  applyTimeBackground,
  backgroundAt,
  backgroundTrack,
  formatHour,
  useTimeBackground,
} from '../../src/web/timeBackground';

function at(hour: number, minute = 0): Date {
  return new Date(2026, 9, 17, hour, minute, 0, 0);
}

const AuroraVars = ['--aurora-1', '--aurora-2', '--aurora-3'] as const;

function clearVars(): void {
  for (const key of AuroraVars) document.documentElement.style.removeProperty(key);
}

beforeEach(() => {
  window.localStorage.clear();
  clearVars();
});

afterEach(() => {
  vi.useRealTimers();
  clearVars();
});

describe('背景时段色板', () => {
  it('锚点时刻取锚点原色：19 点保持现有深/浅背景', () => {
    assert.deepEqual(backgroundAt('dark', at(19)).colors, ['#3A47A8', '#1C6E78', '#6D4FA8']);
    assert.deepEqual(backgroundAt('light', at(19)).colors, ['#B9C6FF', '#A9E2E6', '#D6C4F5']);
  });

  it('锚点之间按分钟连续插值', () => {
    const bg = backgroundAt('dark', at(6, 30));
    assert.equal(bg.label, '上午');
    assert.deepEqual(bg.colors, ['#544783', '#4C545A', '#3E4A88']);
  });

  it('24 小时首尾相连：22 点之后向次日凌晨过渡', () => {
    const bg = backgroundAt('light', at(23, 30));
    assert.equal(bg.label, '凌晨');
    assert.deepEqual(bg.colors, ['#C2B8FD', '#AABBFC', '#C8B1F7']);
  });

  it('把时段色写成 :root 的 CSS 变量', () => {
    applyTimeBackground('dark', at(19));
    const style = document.documentElement.style;
    assert.equal(style.getPropertyValue('--aurora-1'), '#3A47A8');
    assert.equal(style.getPropertyValue('--aurora-2'), '#1C6E78');
    assert.equal(style.getPropertyValue('--aurora-3'), '#6D4FA8');

    applyTimeBackground('light', at(19));
    assert.equal(style.getPropertyValue('--aurora-1'), '#B9C6FF');
    assert.equal(style.getPropertyValue('--aurora-2'), '#A9E2E6');
    assert.equal(style.getPropertyValue('--aurora-3'), '#D6C4F5');
  });

  it('滑块色带覆盖 24 小时，并在末尾回绕到 0 点', () => {
    const dark = backgroundTrack('dark');
    assert.ok(dark.startsWith('linear-gradient(90deg, #343C92 0.0%'));
    assert.ok(dark.includes('#3A47A8 79.2%'));
    assert.ok(dark.endsWith('#343C92 100%)'));
    assert.ok(backgroundTrack('light').startsWith('linear-gradient(90deg, #C7BDFF 0.0%'));
  });

  it('滑块读数写成 HH:MM', () => {
    assert.equal(formatHour(0), '00:00');
    assert.equal(formatHour(8.25), '08:15');
    assert.equal(formatHour(20.5), '20:30');
    assert.equal(formatHour(23.75), '23:45');
    assert.equal(formatHour(24), '00:00');
  });

  it('拖动时段：停表、立即换色并写入偏好', () => {
    vi.useFakeTimers();
    vi.setSystemTime(at(20));
    const first = renderHook(() => useTimeBackground('dark'));

    assert.equal(first.result.current.auto, true);
    assert.equal(first.result.current.hour, 20);
    assert.equal(first.result.current.label, '晚上');

    act(() => {
      first.result.current.setHour(8);
    });

    assert.equal(first.result.current.auto, false);
    assert.equal(first.result.current.hour, 8);
    assert.equal(first.result.current.label, '上午');
    assert.equal(document.documentElement.style.getPropertyValue('--aurora-1'), '#2E4F9C');
    assert.deepEqual(JSON.parse(window.localStorage.getItem('sump.portal.background') ?? 'null'), {
      auto: false,
      hour: 8,
    });

    first.unmount();
    const second = renderHook(() => useTimeBackground('dark'));
    assert.equal(second.result.current.auto, false);
    assert.equal(second.result.current.hour, 8);
  });

  it('跟随时间：每分钟重新取色', () => {
    vi.useFakeTimers();
    vi.setSystemTime(at(20));
    const { result } = renderHook(() => useTimeBackground('dark'));
    const before = document.documentElement.style.getPropertyValue('--aurora-1');

    act(() => {
      vi.setSystemTime(at(21));
      vi.advanceTimersByTime(60_000);
    });

    assert.ok(Math.abs(result.current.hour - (21 + 1 / 60)) < 1e-9);
    assert.notEqual(document.documentElement.style.getPropertyValue('--aurora-1'), before);
  });

  it('开关：关掉冻结在当前时刻，打开恢复跟随', () => {
    vi.useFakeTimers();
    vi.setSystemTime(at(20));
    const { result } = renderHook(() => useTimeBackground('light'));

    act(() => {
      result.current.setAuto(false);
    });
    assert.equal(result.current.auto, false);
    assert.equal(result.current.hour, 20);

    act(() => {
      result.current.setHour(8);
    });
    assert.equal(result.current.hour, 8);

    act(() => {
      result.current.setAuto(true);
    });
    assert.equal(result.current.auto, true);
    assert.equal(result.current.hour, 20);
  });
});
