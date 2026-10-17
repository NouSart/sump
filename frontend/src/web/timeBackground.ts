import { useCallback, useEffect, useLayoutEffect, useState } from 'react';

/**
 * 背景随本地时间变化：只驱动 .aurora 的三层光斑，不参与任何 UI 令牌。
 *
 * 做法与 blog 的时段主题一致——8 个锚点按小时连续插值，24 小时首尾相连，
 * 每分钟重新取色；差别是这里**只改背景**，surface / text / accent 等令牌
 * 一律不动，所以玻璃层级与对比度保持现状。深/浅两套色板各自成环，
 * 切换外观时由当前时刻重新取色。
 */

export type BackgroundTheme = 'light' | 'dark';

type AuroraColors = readonly [string, string, string];

type Anchor = {
  hour: number;
  label: string;
  colors: AuroraColors;
};

const DARK_ANCHORS: readonly Anchor[] = [
  { hour: 0, label: '凌晨', colors: ['#343C92', '#1B3D6E', '#4B3B8C'] },
  { hour: 5, label: '清晨', colors: ['#7A3F6A', '#7C4A3A', '#4C3A86'] },
  { hour: 8, label: '上午', colors: ['#2E4F9C', '#1C5E7A', '#2F5A8A'] },
  { hour: 11, label: '中午', colors: ['#6A5428', '#2A5F8A', '#1C6A78'] },
  { hour: 13, label: '下午', colors: ['#1F6A5A', '#1C6E78', '#2F5A7A'] },
  { hour: 17, label: '傍晚', colors: ['#8A4A32', '#6E3A5E', '#4A3A8A'] },
  { hour: 19, label: '晚上', colors: ['#3A47A8', '#1C6E78', '#6D4FA8'] },
  { hour: 22, label: '午夜', colors: ['#3A3A8A', '#232F6E', '#4A2F7A'] },
];

const LIGHT_ANCHORS: readonly Anchor[] = [
  { hour: 0, label: '凌晨', colors: ['#C7BDFF', '#AEBDFF', '#C9B4FA'] },
  { hour: 5, label: '清晨', colors: ['#FFD2E2', '#FFD3B8', '#E7C8FF'] },
  { hour: 8, label: '上午', colors: ['#B9DCFF', '#BCEBFF', '#B8F0E4'] },
  { hour: 11, label: '中午', colors: ['#FFE7AD', '#C8E7FF', '#C2F1E8'] },
  { hour: 13, label: '下午', colors: ['#BDF0DD', '#B5EEE8', '#CDEEDC'] },
  { hour: 17, label: '傍晚', colors: ['#FFD0A0', '#FFC2D5', '#DDC3FF'] },
  { hour: 19, label: '晚上', colors: ['#B9C6FF', '#A9E2E6', '#D6C4F5'] },
  { hour: 22, label: '午夜', colors: ['#B3A9F5', '#9FB3F2', '#C3A9EF'] },
];

const ANCHORS: Record<BackgroundTheme, readonly Anchor[]> = {
  light: LIGHT_ANCHORS,
  dark: DARK_ANCHORS,
};

export type TimeBackground = {
  label: string;
  colors: AuroraColors;
};

/** 外观设置里的背景时段开关与滑块。 */
export type TimeBackgroundControls = {
  /** 是否跟随本地时间 */
  auto: boolean;
  /** 滑块位置（0~24）：自动时等于当前时刻，手动时等于固定时段 */
  hour: number;
  /** 当前时段名（凌晨 / 清晨 / …） */
  label: string;
  setAuto: (auto: boolean) => void;
  setHour: (hour: number) => void;
};

const PrefKey = 'sump.portal.background';

type Pref = { auto: boolean; hour: number };

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

type RGB = [number, number, number];

function hexToRgb(hex: string): RGB {
  const v = hex.replace('#', '');
  return [
    parseInt(v.slice(0, 2), 16),
    parseInt(v.slice(2, 4), 16),
    parseInt(v.slice(4, 6), 16),
  ];
}

function mixHex(from: string, to: string, t: number): string {
  const a = hexToRgb(from);
  const b = hexToRgb(to);
  const toHex = (n: number) =>
    Math.round(Math.min(255, Math.max(0, n)))
      .toString(16)
      .padStart(2, '0')
      .toUpperCase();
  return `#${toHex(a[0] + (b[0] - a[0]) * t)}${toHex(a[1] + (b[1] - a[1]) * t)}${toHex(a[2] + (b[2] - a[2]) * t)}`;
}

/** 取某一时刻的背景光斑色；24 小时首尾相连，22 点与 0 点之间同样平滑过渡。 */
export function backgroundAt(theme: BackgroundTheme, date = new Date()): TimeBackground {
  const anchors = ANCHORS[theme];
  const hours = date.getHours() + date.getMinutes() / 60;

  let index = 0;
  for (let k = 0; k < anchors.length; k++) {
    if (anchors[k]!.hour <= hours) index = k;
  }

  const from = anchors[index]!;
  const to = anchors[(index + 1) % anchors.length]!;
  const span = (to.hour - from.hour + 24) % 24 || 24;
  const t = ((hours - from.hour + 24) % 24) / span;

  return {
    // 时段名跟随最接近的锚点，避免插值中途出现"半上午半中午"
    label: t < 0.5 ? from.label : to.label,
    colors: [
      mixHex(from.colors[0], to.colors[0], t),
      mixHex(from.colors[1], to.colors[1], t),
      mixHex(from.colors[2], to.colors[2], t),
    ],
  };
}

/** 把当前时段色写成 CSS 变量；样式表里的 .aurora 只消费变量。 */
export function applyTimeBackground(theme: BackgroundTheme, date = new Date()) {
  const { colors } = backgroundAt(theme, date);
  const root = document.documentElement.style;
  root.setProperty('--aurora-1', colors[0]);
  root.setProperty('--aurora-2', colors[1]);
  root.setProperty('--aurora-3', colors[2]);
}

/** 滑块轨道：8 个锚点的主光斑色铺满一天，末尾接回 0 点，拖动时能直接看到会变成什么颜色。 */
export function backgroundTrack(theme: BackgroundTheme): string {
  const anchors = ANCHORS[theme];
  const stops = anchors.map((a) => `${a.colors[0]} ${((a.hour / 24) * 100).toFixed(1)}%`);
  const first = anchors[0]!;
  stops.push(`${first.colors[0]} 100%`);
  return `linear-gradient(90deg, ${stops.join(', ')})`;
}

/** 把 0~24 的滑块值写成 HH:MM（24 点回绕到 00:00）。 */
export function formatHour(hour: number): string {
  const total = Math.round(clampHour(hour) * 60) % (24 * 60);
  const h = Math.floor(total / 60);
  const m = total % 60;
  return `${String(h).padStart(2, '0')}:${String(m).padStart(2, '0')}`;
}

/**
 * 跟随本地时间刷新背景，并把偏好与滑块状态交给设置页。
 * layout effect 保证与首帧同一次绘制；自动档每分钟重新取色，手动档停表。
 */
export function useTimeBackground(theme: BackgroundTheme): TimeBackgroundControls {
  const [pref, setPref] = useState(readPref);
  const [now, setNow] = useState(() => new Date());

  useLayoutEffect(() => {
    applyTimeBackground(theme, pref.auto ? now : dateAtHour(pref.hour));
  }, [theme, pref, now]);

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
    label: backgroundAt(theme, sample).label,
    setAuto,
    setHour,
  };
}
