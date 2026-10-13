import type { ServiceCardView } from '../types';

interface Props {
  services: ServiceCardView[];
  selected: string | null;
  settingsCountOf: (name: string) => number;
  onSelect: (name: string) => void;
}

export function ServiceList({ services, selected, settingsCountOf, onSelect }: Props) {
  if (services.length === 0) {
    return <p className="empty">尚未收到名册——等待设置中心广播。</p>;
  }
  return (
    <ul className="svc-list">
      {services.map((s) => {
        const count = settingsCountOf(s.name);
        return (
          <li key={s.name}>
            <button
              type="button"
              className={selected === s.name ? 'svc-item svc-item-active' : 'svc-item'}
              onClick={() => onSelect(s.name)}
            >
              <span className="svc-name-row">
                <span className="svc-name mono">{s.name}</span>
                {count > 0 && (
                  <span className="badge" title={`${count} 个设置项`}>
                    {count}
                  </span>
                )}
              </span>
              {s.description && <span className="svc-desc">{s.description}</span>}
            </button>
          </li>
        );
      })}
    </ul>
  );
}
