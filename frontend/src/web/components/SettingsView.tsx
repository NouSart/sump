// SettingsView 是设置正文：它占 agent 面板的位置（不是浮层抽屉），
// 所以左边栏、字标、收起按钮全都照旧可见可点——服务导航在左边栏里，不在这里。
// 这里只负责三件事：标题、外观开关、退出设置（Esc 同效）。
import { useEffect } from 'react';
import { IconDark, IconLight } from './icons';
import { SettingsPanel } from './SettingsPanel';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';

interface Props {
  theme: 'light' | 'dark';
  onTheme: (t: 'light' | 'dark') => void;
  onClose: () => void;
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  connected: boolean;
  index: number;
  total: number;
  statusOf: (service: string, key: string) => RowStatus | undefined;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
}

export function SettingsView({
  theme,
  onTheme,
  onClose,
  service,
  settings,
  connected,
  index,
  total,
  statusOf,
  onSave,
  onReset,
}: Props) {
  useEffect(() => {
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  }, [onClose]);

  return (
    <section className="settings" aria-label="设置">
      <header className="settings-head">
        <h1 className="settings-title">设置</h1>
        <span className="grow" />
        <div className="theme-switch" role="radiogroup" aria-label="外观">
          <button
            type="button"
            role="radio"
            aria-checked={theme === 'light'}
            className="theme-tab"
            onClick={() => {
              onTheme('light');
            }}
          >
            <IconLight />
            浅色
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={theme === 'dark'}
            className="theme-tab"
            onClick={() => {
              onTheme('dark');
            }}
          >
            <IconDark />
            深色
          </button>
        </div>
        <button type="button" className="chip" onClick={onClose} aria-label="关闭设置">
          关闭
        </button>
      </header>
      <div className="settings-body">
        <SettingsPanel
          service={service}
          settings={settings}
          connected={connected}
          index={index}
          total={total}
          statusOf={statusOf}
          onSave={onSave}
          onReset={onReset}
        />
      </div>
    </section>
  );
}
