export interface Toast {
  id: number;
  kind: 'ok' | 'error';
  text: string;
}

export function Toasts({ items }: { items: Toast[] }) {
  return (
    <div className="toasts" role="status" aria-live="polite">
      {items.map((t) => (
        <div key={t.id} className={t.kind === 'ok' ? 'toast toast-ok' : 'toast toast-error'}>
          {t.text}
        </div>
      ))}
    </div>
  );
}
