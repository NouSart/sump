import assert from 'node:assert/strict';
import { afterEach, beforeEach, describe, it, vi } from 'vitest';
import { act, renderHook } from '@testing-library/react';
import { applyTimeTheme, formatHour, luminance, themeAt, themeTrack, useTimeTheme } from '../../src/web/timeTheme';

function at(hour: number, minute = 0): Date {
  return new Date(2026, 9, 17, hour, minute, 0, 0);
}

const ThemeVars = [
  '--bg-0',
  '--aurora-1',
  '--aurora-2',
  '--aurora-3',
  '--accent',
  '--accent-soft',
  '--accent-line',
] as const;

function clearVars(): void {
  for (const key of ThemeVars) document.documentElement.style.removeProperty(key);
  document.documentElement.dataset.theme = 'dark';
}

beforeEach(() => {
  window.localStorage.clear();
  clearVars();
});

afterEach(() => {
  vi.useRealTimers();
  clearVars();
});

describe('时段主题', () => {
  it('锚点时刻取锚点原色：中午亮、深夜暗', () => {
    const noon = themeAt(at(12));
    assert.equal(noon.label, '中午');
    assert.equal(noon.base, '#FBF7EA');
    assert.equal(noon.accent, '#B45309');
    assert.equal(noon.isDark, false);

    const night = themeAt(at(22));
    assert.equal(night.label, '深夜');
    assert.equal(night.base, '#0A0C14');
    assert.equal(night.accent, '#A78BFA');
    assert.equal(night.isDark, true);
  });

  it('锚点之间连续插值：底色与强调色都随时间过渡', () => {
    const bg = themeAt(at(10, 30));
    assert.equal(bg.label, '上午');
    assert.equal(bg.base, '#F0F7F7');
    assert.match(bg.accent, /^#[0-9A-F]{6}$/);
    assert.notEqual(bg.accent, '#0369A1');
    assert.notEqual(bg.accent, '#0F766E');
    assert.equal(bg.isDark, false);
  });

  it('24 小时首尾相连：23 点之后向次日午夜过渡', () => {
    const bg = themeAt(at(23, 30));
    assert.equal(bg.label, '午夜');
    assert.equal(bg.base, '#080A12');
    assert.equal(bg.accent, '#8B9CFF');
    assert.equal(bg.isDark, true);
  });

  it('中间调锚点被拉回档位安全区：黄昏/破晓不再拿深色令牌盖中间调底', () => {
    // 19 点锚点原色 #3A2A3A 是中间调（亮度约 49）：深色令牌（半透明白 + 浅字）压不住，
    // 浅字与提示文字会掉到 3:1 以下。themeAt 应把底色与光斑一起拉暗进深色档。
    const dusk = themeAt(at(19));
    assert.equal(dusk.isDark, true);
    assert.ok(luminance(dusk.base) <= 20.5, `黄昏底色应压到深色档安全区，实际亮度 ${String(luminance(dusk.base))}`);
    assert.notEqual(dusk.base, '#3A2A3A', '不再是原样的中间调');
    for (const orb of dusk.orbs) {
      assert.ok(luminance(orb) < 45, `光斑也要压暗，实际亮度 ${String(luminance(orb))}`);
    }

    const dawn = themeAt(at(5));
    assert.equal(dawn.isDark, true);
    assert.ok(luminance(dawn.base) <= 20.5, `破晓底色应压到深色档安全区，实际亮度 ${String(luminance(dawn.base))}`);

    // 本就够暗/够亮的时段不被无谓改动（避免整站发灰）
    assert.equal(themeAt(at(0)).base, '#080A12');
    assert.equal(themeAt(at(12)).base, '#FBF7EA');
    assert.equal(themeAt(at(22)).base, '#0A0C14');
  });

  it('日光档同样兜底：过渡到黄昏之前，底色不会停在灰调', () => {
    const late = themeAt(at(18, 50));
    assert.equal(late.isDark, false, '18:50 仍是日光档');
    assert.ok(luminance(late.base) >= 214, `日光档底色亮度下限 215，实际 ${String(luminance(late.base))}`);
    assert.ok(luminance(late.orbs[0]) > 180, '光斑也不该拖暗');
  });

  it('把整站配色写成 CSS 变量并自动选择深浅令牌', () => {
    applyTimeTheme(at(12));
    const style = document.documentElement.style;
    assert.equal(document.documentElement.dataset.theme, 'light');
    assert.equal(style.getPropertyValue('--bg-0'), '#FBF7EA');
    assert.equal(style.getPropertyValue('--aurora-1'), '#FFE7AD');
    assert.equal(style.getPropertyValue('--accent'), '#B45309');
    assert.equal(style.getPropertyValue('--accent-soft'), 'rgba(180, 83, 9, 0.09)');
    assert.equal(style.getPropertyValue('--accent-line'), 'rgba(180, 83, 9, 0.45)');

    applyTimeTheme(at(22));
    assert.equal(document.documentElement.dataset.theme, 'dark');
    assert.equal(style.getPropertyValue('--bg-0'), '#0A0C14');
    assert.equal(style.getPropertyValue('--accent'), '#A78BFA');
    assert.equal(style.getPropertyValue('--accent-soft'), 'rgba(167, 139, 250, 0.13)');
    assert.equal(style.getPropertyValue('--accent-line'), 'rgba(167, 139, 250, 0.55)');
  });

  it('滑块色带按 15 分钟细分，并在末尾回绕', () => {
    const track = themeTrack();
    assert.ok(track.startsWith('linear-gradient(90deg, #8B9CFF 0.00%'));
    assert.ok(track.includes('#B45309 50.00%'));
    assert.ok(track.endsWith('#8B9CFF 100.00%)'));
    assert.ok(track.split(',').length > 90, '色带应细分到 15 分钟');
  });

  it('滑块读数写成 HH:MM', () => {
    assert.equal(formatHour(0), '00:00');
    assert.equal(formatHour(8.25), '08:15');
    assert.equal(formatHour(23.75), '23:45');
    assert.equal(formatHour(24), '00:00');
  });

  it('拖动时段：停表、立即切换整站主题并写入偏好', () => {
    vi.useFakeTimers();
    vi.setSystemTime(at(20));
    const first = renderHook(() => useTimeTheme());

    assert.equal(first.result.current.auto, true);

    act(() => {
      first.result.current.setHour(12);
    });

    assert.equal(first.result.current.auto, false);
    assert.equal(first.result.current.hour, 12);
    assert.equal(first.result.current.label, '中午');
    assert.equal(document.documentElement.dataset.theme, 'light');
    assert.equal(document.documentElement.style.getPropertyValue('--bg-0'), '#FBF7EA');
    assert.deepEqual(JSON.parse(window.localStorage.getItem('sump.portal.background') ?? 'null'), {
      auto: false,
      hour: 12,
    });

    act(() => {
      first.result.current.setHour(22);
    });
    assert.equal(document.documentElement.dataset.theme, 'dark');
    assert.equal(document.documentElement.style.getPropertyValue('--bg-0'), '#0A0C14');

    first.unmount();
    const second = renderHook(() => useTimeTheme());
    assert.equal(second.result.current.auto, false);
    assert.equal(second.result.current.hour, 22);
  });

  it('跟随时间：每分钟重新取色', () => {
    vi.useFakeTimers();
    vi.setSystemTime(at(20));
    const { result } = renderHook(() => useTimeTheme());
    const before = document.documentElement.style.getPropertyValue('--bg-0');

    act(() => {
      vi.setSystemTime(at(21));
      vi.advanceTimersByTime(60_000);
    });

    assert.ok(Math.abs(result.current.hour - (21 + 1 / 60)) < 1e-9);
    assert.notEqual(document.documentElement.style.getPropertyValue('--bg-0'), before);
  });

  it('开关：关掉冻结在当前时刻，打开恢复跟随', () => {
    vi.useFakeTimers();
    vi.setSystemTime(at(12));
    const { result } = renderHook(() => useTimeTheme());

    act(() => {
      result.current.setAuto(false);
    });
    assert.equal(result.current.auto, false);
    assert.equal(result.current.hour, 12);
    assert.equal(document.documentElement.dataset.theme, 'light');

    act(() => {
      result.current.setHour(22);
    });
    assert.equal(result.current.hour, 22);
    assert.equal(document.documentElement.dataset.theme, 'dark');

    act(() => {
      result.current.setAuto(true);
    });
    assert.equal(result.current.auto, true);
    assert.equal(result.current.hour, 12);
    assert.equal(document.documentElement.dataset.theme, 'light');
  });
});
