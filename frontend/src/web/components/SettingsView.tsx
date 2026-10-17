// SettingsView 是设置正文：它占 agent 面板的位置（不是浮层抽屉），
// 所以左边栏、字标、收起按钮全都照旧可见可点——服务导航在左边栏里，不在这里。
// 这里只负责：标题、语言、外观、退出设置（Esc 同效）。
import { useEffect, type CSSProperties } from 'react';
import { IconClose, IconMoon, IconSun } from './icons';
import { SettingsPanel } from './SettingsPanel';
import { backgroundTrack, formatHour, type TimeBackgroundControls } from '../timeBackground';
import type { Translate } from '../i18n';
import type { LocaleInfo } from '../i18n';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';

interface Props {
  theme: 'light' | 'dark';
  onTheme: (t: 'light' | 'dark') => void;
  timeBackground: TimeBackgroundControls;
  onClose: () => void;
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  /** 名册里的服务名：给"服务名"这类设置项当候选。 */
  peers: string[];
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
  timeBackground,
  onClose,
  service,
  settings,
  peers,
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
        <section className="appearance" aria-label={t('ui.settings.background', '背景')}>
          <h2 className="set-label">{t('ui.settings.background', '背景')}</h2>
          <div className="form">
            <div className="field">
              <div className="field-head">
                <span className="field-label">{t('ui.settings.followTime', '跟随时间')}</span>
                <span className="field-badges">
                  <span className={timeBackground.auto ? 'tag-default' : 'tag-over'}>
                    {timeBackground.auto
                      ? t('ui.settings.following', '跟随中')
                      : t('ui.settings.fixed', '已固定')}
                  </span>
                </span>
              </div>
              <div className="field-control">
                <button
                  type="button"
                  className="switch"
                  role="switch"
                  aria-checked={timeBackground.auto}
                  aria-label={t('ui.settings.followTime', '跟随时间')}
                  onClick={() => {
                    timeBackground.setAuto(!timeBackground.auto);
                  }}
                >
                  <span className="switch-knob" />
                </button>
              </div>
              <p className="field-hint">
                {t(
                  'ui.settings.followTimeHint',
                  '每分钟按本地时间重新取色，背景连续过渡，不会到点突跳。',
                )}
              </p>
            </div>

            <div className="field">
              <div className="field-head">
                <span className="field-label">{t('ui.settings.backgroundTime', '背景时段')}</span>
                <span className="field-badges">
                  <span className="tag-over">{timeBackground.label}</span>
                </span>
              </div>
              <div className="field-control">
                <input
                  type="range"
                  className="range"
                  min={0}
                  max={24}
                  step={0.25}
                  value={timeBackground.hour}
                  aria-label={t('ui.settings.backgroundTime', '背景时段')}
                  aria-valuetext={`${formatHour(timeBackground.hour)} · ${timeBackground.label}`}
                  style={{ '--range-track': backgroundTrack(theme) } as CSSProperties}
                  onChange={(e) => {
                    timeBackground.setHour(Number(e.target.value));
                  }}
                />
                <span className="field-readout">{formatHour(timeBackground.hour)}</span>
              </div>
              <p className="field-hint">
                {timeBackground.auto
                  ? `${t('ui.settings.followingNow', '跟随时间中 · 当前')}「${timeBackground.label}」；${t('ui.settings.dragToFix', '拖动滑块会固定到所选时段')}。`
                  : `${t('ui.settings.fixedAt', '已固定为')}「${timeBackground.label}」；${t('ui.settings.reopenAuto', '打开「跟随时间」恢复自动')}。`}
              </p>
            </div>
          </div>
        </section>
        <SettingsPanel
          service={service}
          settings={settings}
          peers={peers}
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