import type { ServiceCardView } from '../types';

interface Props {
  services: ServiceCardView[];
  selected: string | null;
  settingsCountOf: (name: string) => number;
  onSelect: (name: string) => void;
}

export function ServiceList({ services, selected, settingsCountOf, onSelect }: Props) {
  if (services.length === 0) {
    return <p className="empty empty-side">尚未收到名册</p>;
  }
  return (
    <ul className="svc-list">
      {services.map((s, i) => {
        const count = settingsCountOf(s.name);
        return (
          <li key={s.name}>
            <button
              type="button"
              className={selected === s.name ? 'svc-item svc-item-active' : 'svc-item'}
              onClick={() => onSelect(s.name)}
              title={s.description ?? s.name}
            >
              <span className="svc-idx">{String(i + 1).padStart(2, '0')}</span>
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
