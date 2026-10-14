import type { ServiceCardView } from '../types';

interface Props {
  services: ServiceCardView[];
  selected: string | null;
  settingsCountOf: (name: string) => number;
  onSelect: (name: string) => void;
}

export function ServiceList({ services, selected, settingsCountOf, onSelect }: Props) {
  if (services.length === 0) {
    return <p className="empty-side">尚未收到名册</p>;
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
              title={s.description ?? s.name}
            >
              <i aria-hidden="true" />
              <span className="svc-name">{s.name}</span>
              {count > 0 && (
                <span className="badge" title={`${count} 个设置项`}>
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
