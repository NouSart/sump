import { useState } from 'react';
import type { SettingViewData, ServiceCardView } from '../types';

interface Props {
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  connected: boolean;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
}

function provideTitle(input?: string, output?: string): string {
  return [input ? `输入：${input}` : '', output ? `输出：${output}` : ''].filter(Boolean).join(' / ');
}

export function SettingsPanel({ service, settings, connected, onSave, onReset }: Props) {
  if (!service) {
    return <div className="empty">尚未收到名册——等待设置中心广播，或检查面板与中心的连接。</div>;
  }
  return (
    <div className="panel">
      <header className="panel-head">
        <h2 className="panel-title mono">{service.name}</h2>
        {service.description && <p className="panel-desc">{service.description}</p>}
        <p className="panel-addr mono muted">{service.addr}</p>
        {(service.provides?.length ?? 0) > 0 && (
          <div className="chips">
            {service.provides?.map((p) => (
              <span key={p.action} className={p.tool ? 'chip chip-tool' : 'chip'} title={provideTitle(p.input, p.output)}>
                {p.action}
                {p.tool && <span className="chip-tag">工具</span>}
              </span>
            ))}
          </div>
        )}
      </header>

      <p className="section-label">设置项</p>
      {settings === null ? (
        <div className="empty">加载中…</div>
      ) : settings.length === 0 ? (
        <div className="empty">该服务未声明可设置项。面板只显示服务真实声明并消费的设置（不做假旋钮）。</div>
      ) : (
        <div className="card table-scroll">
          <table className="settings-table">
            <thead>
              <tr>
                <th scope="col">键</th>
                <th scope="col">生效值</th>
                <th scope="col">默认值</th>
                <th scope="col">状态</th>
                <th scope="col" className="col-actions">
                  操作
                </th>
              </tr>
            </thead>
            <tbody>
              {settings.map((item) => (
                <SettingRow
                  key={item.key}
                  service={service.name}
                  item={item}
                  connected={connected}
                  onSave={onSave}
                  onReset={onReset}
                />
              ))}
            </tbody>
          </table>
        </div>
      )}
    </div>
  );
}

interface RowProps {
  service: string;
  item: SettingViewData;
  connected: boolean;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
}

function SettingRow({ service, item, connected, onSave, onReset }: RowProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState(item.value);
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  const startEdit = (): void => {
    setDraft(item.value);
    setEditing(true);
    setConfirming(false);
  };

  const save = async (): Promise<void> => {
    if (draft === item.value) {
      setEditing(false);
      return;
    }
    setBusy(true);
    try {
      await onSave(service, item.key, draft);
      setEditing(false);
    } catch {
      // 失败提示由上层 toast 负责；保持编辑态让用户可修正
    } finally {
      setBusy(false);
    }
  };

  const reset = async (): Promise<void> => {
    if (!confirming) {
      setConfirming(true);
      setTimeout(() => setConfirming(false), 3000);
      return;
    }
    setBusy(true);
    try {
      await onReset(service, item.key);
      setConfirming(false);
    } catch {
      // 同上
    } finally {
      setBusy(false);
    }
  };

  return (
    <tr>
      <td className="mono">{item.key}</td>
      <td>
        {editing ? (
          <input
            className="input"
            value={draft}
            aria-label={`${item.key} 的新值`}
            onChange={(e) => setDraft(e.target.value)}
            onKeyDown={(e) => {
              if (e.key === 'Enter') void save();
              if (e.key === 'Escape') setEditing(false);
            }}
            autoFocus
          />
        ) : (
          <span className="mono">{item.value || '—'}</span>
        )}
      </td>
      <td>
        <span className="mono muted">{item.default || '—'}</span>
      </td>
      <td>{item.overridden ? <span className="badge badge-warn">已覆盖</span> : <span className="badge badge-quiet">默认</span>}</td>
      <td className="col-actions">
        {editing ? (
          <>
            <button type="button" className="btn primary" onClick={() => void save()} disabled={busy || !connected}>
              保存
            </button>
            <button type="button" className="btn ghost" onClick={() => setEditing(false)} disabled={busy}>
              取消
            </button>
          </>
        ) : (
          <>
            <button type="button" className="btn" onClick={startEdit} disabled={busy || !connected}>
              修改
            </button>
            {item.overridden && (
              <button
                type="button"
                className={confirming ? 'btn danger' : 'btn ghost'}
                onClick={() => void reset()}
                disabled={busy || !connected}
              >
                {confirming ? '确认重置' : '重置'}
              </button>
            )}
          </>
        )}
      </td>
    </tr>
  );
}
