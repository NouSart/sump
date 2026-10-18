// 语言包：文案全部走 locales/<locale>.json（门户启动时读一次，浏览器可按需切换）。
// 服务声明的中文是**兜底**——包里查不到就用服务给的那句，再查不到才露出键名，
// 于是「切到全 x 的测试包后还能看见中文」= 这一处漏了翻译。
import { useEffect, useState } from 'react';

export interface Pack {
  locale: string;
  name: string;
  messages: Record<string, string>;
}

export interface LocaleInfo {
  locale: string;
  name: string;
}

const LocaleKey = 'sump.portal.locale';
export const DefaultLocale = 'zh-CN';

export function initialLocale(): string {
  const saved = window.localStorage.getItem(LocaleKey);
  return saved !== null && saved !== '' ? saved : DefaultLocale;
}

export function rememberLocale(locale: string): void {
  window.localStorage.setItem(LocaleKey, locale);
}

async function getJSON<T>(path: string): Promise<T> {
  const res = await fetch(path);
  if (!res.ok) throw new Error(`HTTP ${String(res.status)}`);
  return (await res.json()) as T;
}

export function fetchLocales(): Promise<LocaleInfo[]> {
  return getJSON<{ locales?: LocaleInfo[] }>('/api/i18n/locales').then((r) => r.locales ?? []);
}

export function fetchPack(locale: string): Promise<Pack> {
  return getJSON<Pack>(`/api/i18n?lang=${encodeURIComponent(locale)}`);
}

// t 的签名：键 + 兜底文案。包里没有该键时用兜底，兜底也没有就露键名（便于发现漏配）。
export type Translate = (key: string, fallback?: string) => string;

export function translator(pack: Pack | null): Translate {
  return (key, fallback) => {
    const hit = pack?.messages[key];
    if (hit !== undefined && hit !== '') return hit;
    return fallback ?? key;
  };
}

// useLocalePack 负责“当前语言 + 包”的小状态机：切换即拉新包，失败保留旧包（界面不白）。
export function useLocalePack(): {
  locale: string;
  pack: Pack | null;
  locales: LocaleInfo[];
  setLocale: (locale: string) => void;
  t: Translate;
} {
  const [locale, setLocaleState] = useState<string>(initialLocale);
  const [pack, setPack] = useState<Pack | null>(null);
  const [locales, setLocales] = useState<LocaleInfo[]>([]);

  useEffect(() => {
    let cancelled = false;
    void fetchLocales()
      .then((list) => {
        if (!cancelled) setLocales(list);
      })
      .catch(() => {
        // 语言包接口不可用不是致命问题：界面退回兜底文案
      });
    return () => {
      cancelled = true;
    };
  }, []);

  useEffect(() => {
    let cancelled = false;
    void fetchPack(locale)
      .then((p) => {
        if (!cancelled) setPack(p);
      })
      .catch(() => {
        // 同上：保留旧包/兜底
      });
    return () => {
      cancelled = true;
    };
  }, [locale]);

  const setLocale = (next: string): void => {
    rememberLocale(next);
    setLocaleState(next);
  };

  return { locale, pack, locales, setLocale, t: translator(pack) };
}