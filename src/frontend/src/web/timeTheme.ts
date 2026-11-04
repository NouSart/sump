import { useCallback, useEffect, useLayoutEffect, useState } from 'react';

/**
 * 时段主题：整站配色（整体底色、背景光斑、强调色、深/浅面板）都随本地时间变化。
 *
 * 17 个锚点按小时连续插值；底色走 RGB（保持自然），光斑与强调色走 HSL
 * （经过饱和的中间色，而不是发灰的 RGB 中点）。底色亮度跨过暗端阈值时自动
 * 切换 `data-theme` 的深/浅两套文字与面板令牌。文字不参与插值——否则过渡
 * 途中必然出现"灰底灰字"。
 */

export type TimeTheme = {
  label: string;
  /** 页面整体底色（不只是气泡） */
  base: string;
  /** 三层背景光斑 */
  orbs: readonly [string, string, string];
  /** 全局强调色 */
  accent: string;
  /** 当前底色是否属于深色档 */
  isDark: boolean;
};

type Anchor = {
  hour: number;
  label: string;
  base: string;
  orbs: readonly [string, string, string];
  accent: string;
};

const ANCHORS: readonly Anchor[] = [
  { hour: 0, label: '午夜', base: '#080A12', orbs: ['#343C92', '#1B3D6E', '#4B3B8C'], accent: '#8B9CFF' },
  { hour: 2, label: '凌晨', base: '#0A0C18', orbs: ['#3A2F7A', '#263A66', '#4A2F6E'], accent: '#A78BFA' },
  { hour: 4, label: '黎明', base: '#12101C', orbs: ['#4A2F6E', '#3A2F55', '#5A3550'], accent: '#C084FC' },
  { hour: 5, label: '破晓', base: '#2A1F2E', orbs: ['#5A3F66', '#7A5544', '#4A3A7A'], accent: '#E879F9' },
  { hour: 6, label: '清晨', base: '#F3E3EA', orbs: ['#FFD2E2', '#FFD3B8', '#E7C8FF'], accent: '#C0266D' },
  { hour: 7, label: '日出', base: '#FFF1E6', orbs: ['#FFD9B0', '#FFC2D5', '#FDE68A'], accent: '#C2410C' },
  { hour: 9, label: '上午', base: '#EEF5FB', orbs: ['#B9DCFF', '#BCEBFF', '#B8F0E4'], accent: '#0369A1' },
  { hour: 11, label: '上午', base: '#F1F8F5', orbs: ['#C7F9E5', '#B9E6FF', '#D9F99D'], accent: '#0F766E' },
  { hour: 12, label: '中午', base: '#FBF7EA', orbs: ['#FFE7AD', '#C8E7FF', '#C2F1E8'], accent: '#B45309' },
  { hour: 14, label: '下午', base: '#EDF6F2', orbs: ['#BDF0DD', '#B5EEE8', '#CDEEDC'], accent: '#0F766E' },
  { hour: 16, label: '午后', base: '#F7F3E8', orbs: ['#FDE68A', '#A7F3D0', '#BAE6FD'], accent: '#A16207' },
  { hour: 17, label: '傍晚', base: '#F8EEE7', orbs: ['#FFD0A0', '#FFC2D5', '#DDC3FF'], accent: '#C2410C' },
  { hour: 18, label: '日落', base: '#F6E2DA', orbs: ['#FB923C', '#F472B6', '#C084FC'], accent: '#BE123C' },
  { hour: 19, label: '黄昏', base: '#3A2A3A', orbs: ['#7C3AED', '#DB2777', '#312E81'], accent: '#F472B6' },
  { hour: 20, label: '暮色', base: '#1A1626', orbs: ['#4C1D95', '#312E81', '#6D28D9'], accent: '#C084FC' },
  { hour: 22, label: '深夜', base: '#0A0C14', orbs: ['#343C92', '#1B3D6E', '#4B3B8C'], accent: '#A78BFA' },
  { hour: 23, label: '深夜', base: '#080A12', orbs: ['#343C92', '#1B3D6E', '#4B3B8C'], accent: '#8B9CFF' },
];

/**
 * 底色亮度阈值（0~255 简单加权）。取在暗端而不是中点：
 * 深色面板只在真正的深底上启用，浅色面板（62% 白）则始终托得住深色文字，
 * 这样开关两个方向的对比度都不会掉到不可读。
 */
const DARK_THRESHOLD = 64;

/**
 * 两套面板令牌各自能"托住"的底色范围：
 * 深色令牌是给近黑底设计的（半透明白面 + 浅字），底色一进中间调，浅字就掉到 3:1 以下；
 * 浅色令牌反过来的问题一样。所以选定档位后把底色**拉回该档的安全区**，
 * 而不是指望两套令牌覆盖整个亮度轴。
 */
const DARK_BASE_MAX_LUM = 20;
const LIGHT_BASE_MIN_LUM = 215;
/** 收暗/提亮用的目标色：都是低饱和的近黑 / 近白，拉过去只改明度，不改色相。 */
const VEIL_DARK = '#0A0A10';
const VEIL_LIGHT = '#FBFAFD';

const PrefKey = 'sump.portal.background';

type Pref = { auto: boolean; hour: number };

export type TimeThemeControls = {
  /** 是否跟随本地时间 */
  auto: boolean;
  /** 滑块位置（0~24）：自动时等于当前时刻，手动时等于固定时段 */
  hour: number;
  /** 当前时段名（凌晨 / 清晨 / …） */
  label: string;
  setAuto: (auto: boolean) => void;
  setHour: (hour: number) => void;
};

type RGB = [number, number, number];

function hexToRgb(hex: string): RGB {
  const v = hex.replace('#', '');
  return [
    parseInt(v.slice(0, 2), 16),
    parseInt(v.slice(2, 4), 16),
    parseInt(v.slice(4, 6), 16),
  ];
}

function rgbToHex([r, g, b]: RGB): string {
  const to = (n: number) =>
    Math.round(Math.min(255, Math.max(0, n)))
      .toString(16)
      .padStart(2, '0')
      .toUpperCase();
  return `#${to(r)}${to(g)}${to(b)}`;
}

function mixHex(from: string, to: string, t: number): string {
  const a = hexToRgb(from);
  const b = hexToRgb(to);
  return rgbToHex([
    a[0] + (b[0] - a[0]) * t,
    a[1] + (b[1] - a[1]) * t,
    a[2] + (b[2] - a[2]) * t,
  ]);
}

type HSL = { h: number; s: number; l: number };

function rgbToHsl([r, g, b]: RGB): HSL {
  const rn = r / 255;
  const gn = g / 255;
  const bn = b / 255;
  const max = Math.max(rn, gn, bn);
  const min = Math.min(rn, gn, bn);
  const l = (max + min) / 2;
  if (max === min) return { h: 0, s: 0, l };
  const d = max - min;
  const s = l > 0.5 ? d / (2 - max - min) : d / (max + min);
  let h: number;
  if (max === rn) h = (gn - bn) / d + (gn < bn ? 6 : 0);
  else if (max === gn) h = (bn - rn) / d + 2;
  else h = (rn - gn) / d + 4;
  return { h: h * 60, s, l };
}

function hslToRgb({ h, s, l }: HSL): RGB {
  const c = (1 - Math.abs(2 * l - 1)) * s;
  const hp = h / 60;
  const x = c * (1 - Math.abs((hp % 2) - 1));
  let rgb: [number, number, number];
  if (hp < 1) rgb = [c, x, 0];
  else if (hp < 2) rgb = [x, c, 0];
  else if (hp < 3) rgb = [0, c, x];
  else if (hp < 4) rgb = [0, x, c];
  else if (hp < 5) rgb = [x, 0, c];
  else rgb = [c, 0, x];
  const m = l - c / 2;
  return [(rgb[0] + m) * 255, (rgb[1] + m) * 255, (rgb[2] + m) * 255];
}

/** 色相层走 HSL：光斑与强调色经过饱和的中间色，而不是发灰的 RGB 中点。 */
function mixHexHsl(from: string, to: string, t: number): string {
  const a = rgbToHsl(hexToRgb(from));
  const b = rgbToHsl(hexToRgb(to));
  let dh = b.h - a.h;
  if (dh > 180) dh -= 360;
  if (dh < -180) dh += 360;
  return rgbToHex(
    hslToRgb({
      h: (a.h + dh * t + 360) % 360,
      s: a.s + (b.s - a.s) * t,
      l: a.l + (b.l - a.l) * t,
    }),
  );
}

function alpha(hex: string, a: number): string {
  const [r, g, b] = hexToRgb(hex);
  return `rgba(${r}, ${g}, ${b}, ${a})`;
}

/** 简单加权亮度，只用来决定深/浅档位。 */
export function luminance(hex: string): number {
  const [r, g, b] = hexToRgb(hex);
  return 0.299 * r + 0.587 * g + 0.114 * b;
}

function hourOf(date: Date): number {
  return date.getHours() + date.getMinutes() / 60;
}

function nowHour(): number {
  return hourOf(new Date());
}

function clampHour(hour: number): number {
  return Math.min(24, Math.max(0, hour));
}

/** 固定时段用的取样时刻（24 点回绕到 0 点）。 */
function dateAtHour(hour: number): Date {
  const d = new Date();
  d.setHours(Math.floor(hour) % 24, Math.round((hour % 1) * 60), 0, 0);
  return d;
}

/** 读偏好：默认跟随本地时间；localStorage 不可用时同样退回默认值。 */
function readPref(): Pref {
  const fallback = { auto: true, hour: nowHour() };
  try {
    const raw = window.localStorage.getItem(PrefKey);
    if (!raw) return fallback;
    const saved = JSON.parse(raw) as Partial<Pref>;
    if (typeof saved.hour !== 'number' || !Number.isFinite(saved.hour)) return fallback;
    return { auto: saved.auto !== false, hour: clampHour(saved.hour) };
  } catch {
    return fallback;
  }
}

/** 取某一时刻的整站配色；24 小时首尾相连，23 点与 0 点之间同样平滑过渡。 */
export function themeAt(date = new Date()): TimeTheme {
  const hours = hourOf(date);

  let index = 0;
  for (let k = 0; k < ANCHORS.length; k++) {
    if (ANCHORS[k]!.hour <= hours) index = k;
  }

  const from = ANCHORS[index]!;
  const to = ANCHORS[(index + 1) % ANCHORS.length]!;
  const span = (to.hour - from.hour + 24) % 24 || 24;
  const t = ((hours - from.hour + 24) % 24) / span;
  const base = mixHex(from.base, to.base, t);
  const isDark = luminance(base) < DARK_THRESHOLD;

  // 把底色与光斑一起拉进当前档位的安全区：光斑会把面板背后照亮，
  // 只压底色不压光斑，浅字照样糊在亮斑上（黄昏那一段最明显）。
  const target = isDark ? DARK_BASE_MAX_LUM : LIGHT_BASE_MIN_LUM;
  const veil = isDark ? VEIL_DARK : VEIL_LIGHT;
  const lum = luminance(base);
  const ratio =
    isDark && lum > target
      ? (lum - target) / (lum - luminance(veil))
      : !isDark && lum < target
        ? (target - lum) / (luminance(veil) - lum)
        : 0;
  const tint = (color: string): string => (ratio > 0 ? mixHex(color, veil, Math.min(1, ratio)) : color);

  return {
    // 时段名跟随最接近的锚点，避免插值中途出现"半上午半中午"
    label: t < 0.5 ? from.label : to.label,
    base: tint(base),
    orbs: [
      tint(mixHexHsl(from.orbs[0], to.orbs[0], t)),
      tint(mixHexHsl(from.orbs[1], to.orbs[1], t)),
      tint(mixHexHsl(from.orbs[2], to.orbs[2], t)),
    ],
    accent: mixHexHsl(from.accent, to.accent, t),
    isDark,
  };
}

/** 把时段配色写成 CSS 变量，并自动选择深/浅两套面板令牌。 */
export function applyTimeTheme(date = new Date()) {
  const theme = themeAt(date);
  const root = document.documentElement;
  root.dataset.theme = theme.isDark ? 'dark' : 'light';

  const style = root.style;
  style.setProperty('--bg-0', theme.base);
  style.setProperty('--aurora-1', theme.orbs[0]);
  style.setProperty('--aurora-2', theme.orbs[1]);
  style.setProperty('--aurora-3', theme.orbs[2]);
  style.setProperty('--accent', theme.accent);
  style.setProperty('--accent-soft', alpha(theme.accent, theme.isDark ? 0.13 : 0.09));
  style.setProperty('--accent-line', alpha(theme.accent, theme.isDark ? 0.55 : 0.45));
}

let trackCache: string | null = null;

/**
 * 滑块轨道：每 15 分钟取一次当前强调色（色相走 HSL），
 * 97 段细色带能看清一天的颜色走向，而不是几个大色块。
 */
export function themeTrack(): string {
  if (trackCache !== null) return trackCache;
  const stops: string[] = [];
  for (let minute = 0; minute < 24 * 60; minute += 15) {
    const accent = themeAt(dateAtHour(minute / 60)).accent;
    stops.push(`${accent} ${((minute / (24 * 60)) * 100).toFixed(2)}%`);
  }
  stops.push(`${themeAt(dateAtHour(0)).accent} 100.00%`);
  trackCache = `linear-gradient(90deg, ${stops.join(', ')})`;
  return trackCache;
}

/** 把 0~24 的滑块值写成 HH:MM（24 点回绕到 00:00）。 */
export function formatHour(hour: number): string {
  const total = Math.round(clampHour(hour) * 60) % (24 * 60);
  const h = Math.floor(total / 60);
  const m = total % 60;
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`;
}

/**
 * 跟随本地时间刷新整站配色，并把偏好与滑块状态交给设置页。
 * layout effect 保证与首帧同一次绘制；自动档每分钟重新取色，手动档停表。
 */
export function useTimeTheme(): TimeThemeControls {
  const [pref, setPref] = useState(readPref);
  const [now, setNow] = useState(() => new Date());

  useLayoutEffect(() => {
    applyTimeTheme(pref.auto ? now : dateAtHour(pref.hour));
  }, [pref, now]);

  useEffect(() => {
    if (!pref.auto) return;
    const id = window.setInterval(() => setNow(new Date()), 60_000);
    return () => window.clearInterval(id);
  }, [pref.auto]);

  useEffect(() => {
    try {
      window.localStorage.setItem(PrefKey, JSON.stringify(pref));
    } catch {
      // 隐私模式 / 配额满：记不住就算了，不影响本次使用
    }
  }, [pref]);

  // 打开跟随 = 跟上现在；关掉 = 冻结在当前时刻（而不是回到上次手动的旧位置）。
  const setAuto = useCallback((auto: boolean) => {
    const at = new Date();
    setNow(at);
    setPref({ auto, hour: hourOf(at) });
  }, []);

  const setHour = useCallback((hour: number) => {
    setPref({ auto: false, hour: clampHour(hour) });
  }, []);

  const sample = pref.auto ? now : dateAtHour(pref.hour);
  return {
    auto: pref.auto,
    hour: pref.auto ? hourOf(now) : pref.hour,
    label: themeAt(sample).label,
    setAuto,
    setHour,
  };
}
