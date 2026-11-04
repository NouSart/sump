import type { ServiceCardView } from '../types';
import type { Translate } from '../i18n';

interface Props {
  services: ServiceCardView[];
  selected: string | null;
  settingsCountOf: (name: string) => number;
  onSelect: (name: string) => void;
  t: Translate;
}

// ServiceList 是设置中心的导航：中文短名在前（服务自己报的），服务名退成灰字。
export function ServiceList({ services, selected, settingsCountOf, onSelect, t }: Props) {
  if (services.length === 0) {
    return <p className="empty-side">{t('ui.settings.emptyAll', '还没有收到任何服务的设置声明')}</p>;
  }
  return (
    <ul className="svc-list">
      {services.map((s) => {
        const count = settingsCountOf(s.name);
        const title = t(`service.${s.name}.title`, s.title ?? s.name);
        return (
          <li key={s.name}>
            <button
              type="button"
              className={selected === s.name ? 'svc-item svc-item-active' : 'svc-item'}
              onClick={() => onSelect(s.name)}
              title={s.description ?? s.name}
            >
              <span className="svc-name">{title}</span>
              {title !== s.name && <span className="svc-id">{s.name}</span>}
              {count > 0 && (
                <span className="badge" title={`${String(count)}${t('ui.settings.badge', ' 个设置项')}`}>
                  {count}
                </span>
              )}
            </button>
          </li>
        );
      })}
    </ul>
  );
}