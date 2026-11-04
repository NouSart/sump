// SettingsView 是设置正文：它占 agent 面板的位置（不是浮层抽屉），
// 所以左边栏、字标、收起按钮全都照旧可见可点——服务导航在左边栏里，不在这里。
// 这里只负责：标题、语言、退出设置（Esc 同效）；「门户」页的外观设置由 TimeThemePanel 注入。
import { useEffect } from 'react';
import { IconClose } from './icons';
import { SettingsPanel } from './SettingsPanel';
import { TimeThemePanel } from './TimeThemePanel';
import { WorkflowExpandPanel } from './WorkflowExpandPanel';
import { BubblePanel } from './BubblePanel';
import type { TimeThemeControls } from '../timeTheme';
import type { WorkflowExpandControls } from '../workflowExpand';
import type { BubbleControls } from '../bubblePrefs';
import type { Translate } from '../i18n';
import type { LocaleInfo } from '../i18n';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';

interface Props {
  timeTheme: TimeThemeControls;
  workflowExpand: WorkflowExpandControls;
  bubble: BubbleControls;
  portalName: string | null;
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
  timeTheme,
  workflowExpand,
  bubble,
  portalName,
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
          peers={peers}
          connected={connected}
          statusOf={statusOf}
          onSave={onSave}
          onReset={onReset}
          portalExtra={
            portalName !== null && service?.name === portalName ? (
              <>
                <WorkflowExpandPanel workflowExpand={workflowExpand} t={t} />
                <BubblePanel bubble={bubble} t={t} />
                <TimeThemePanel timeTheme={timeTheme} t={t} />
              </>
            ) : undefined
          }
          t={t}
        />
      </div>
    </section>
  );
}