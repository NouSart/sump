// SettingsView 是设置正文：它占 agent 面板的位置（不是浮层抽屉），
// 所以左边栏、字标、收起按钮全都照旧可见可点——服务导航在左边栏里，不在这里。
// 这里只负责：标题、语言、外观、退出设置（Esc 同效）。
import { useEffect } from 'react';
import { IconClose, IconMoon, IconSun } from './icons';
import { SettingsPanel } from './SettingsPanel';
import type { Translate } from '../i18n';
import type { LocaleInfo } from '../i18n';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';

interface Props {
  theme: 'light' | 'dark';
  onTheme: (t: 'light' | 'dark') => void;
  onClose: () => void;
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  connected: boolean;
  statusOf: (service: string, key: string) => RowStatus | undefined;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
  locale: string;
  locales: LocaleInfo[];
  onLocale: (locale: string) => void;
  t: Translate;
}

export function SettingsView({
  theme,
  onTheme,
  onClose,
  service,
  settings,
  connected,
  statusOf,
  onSave,
  onReset,
  locale,
  locales,
  onLocale,
  t,
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
    <section className="settings" aria-label={t('ui.settings.title', '设置')}>
      <header className="settings-head">
        <h1 className="settings-title">{t('ui.settings.title', '设置')}</h1>
        <span className="grow" />
        {locales.length > 1 && (
          <select
            className="locale-select"
            aria-label={t('ui.settings.language', '语言')}
            title={t('ui.settings.language', '语言')}
            value={locale}
            onChange={(e) => {
              onLocale(e.target.value);
            }}
          >
            {locales.map((l) => (
              <option key={l.locale} value={l.locale}>
                {l.name}
              </option>
            ))}
          </select>
        )}
        <div className="theme-switch" role="radiogroup" aria-label={t('ui.settings.appearance', '外观')}>
          <button
            type="button"
            role="radio"
            aria-checked={theme === 'light'}
            className="theme-tab"
            aria-label={t('ui.settings.light', '浅色')}
            title={t('ui.settings.light', '浅色')}
            onClick={() => {
              onTheme('light');
            }}
          >
            <IconSun />
          </button>
          <button
            type="button"
            role="radio"
            aria-checked={theme === 'dark'}
            className="theme-tab"
            aria-label={t('ui.settings.dark', '深色')}
            title={t('ui.settings.dark', '深色')}
            onClick={() => {
              onTheme('dark');
            }}
          >
            <IconMoon />
          </button>
        </div>
        <button
          type="button"
          className="chip"
          onClick={onClose}
          aria-label={t('ui.settings.close', '关闭设置')}
          title={t('ui.settings.close', '关闭设置')}
        >
          <IconClose />
        </button>
      </header>
      <div className="settings-body">
        <SettingsPanel
          service={service}
          settings={settings}
          connected={connected}
          statusOf={statusOf}
          onSave={onSave}
          onReset={onReset}
          t={t}
        />
      </div>
    </section>
  );
}