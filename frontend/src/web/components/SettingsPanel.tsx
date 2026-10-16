import { useState } from 'react';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';

interface Props {
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  connected: boolean;
  index: number;
  total: number;
  statusOf: (service: string, key: string) => RowStatus | undefined;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
}

export function SettingsPanel({ service, settings, connected, index, total, statusOf, onSave, onReset }: Props) {
  if (!service) {
    return (
      <div className="panel">
        <p className="empty">尚未收到名册——等待设置中心广播，或检查面板与中心的连接。</p>
      </div>
    );
  }
  const no = String(index).padStart(2, '0');
  const den = String(total).padStart(2, '0');
  return (
    <div className="panel">
      <p className="eyebrow">
        服务 · {no} / {den}
      </p>
      <h1 className="panel-title">{service.name}</h1>
      {service.description && <p className="panel-desc">{service.description}</p>}
      <p className="panel-addr">{service.addr}</p>

      <hr className="rule" />
      <h2 className="set-label">设置项</h2>

      {settings === null ? (
        <p className="empty">加载中…</p>
      ) : settings.length === 0 ? (
        <p className="empty">该服务未声明可设置项——只显示服务真实声明并消费的设置（不做假旋钮）。</p>
      ) : (
        <div className="set-table">
          <div className="set-headrow" aria-hidden="true">
            <span>键</span>
            <span>生效值</span>
            <span>默认值</span>
            <span>状态</span>
            <span className="head-actions">操作</span>
          </div>
          {settings.map((item) => (
            <SettingRow
              key={item.key}
              service={service.name}
              item={item}
              status={statusOf(service.name, item.key)}
              connected={connected}
              onSave={onSave}
              onReset={onReset}
            />
          ))}
        </div>
      )}
    </div>
  );
}

interface RowProps {
  service: string;
  item: SettingViewData;
  status?: RowStatus;
  connected: boolean;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
}

function SettingRow({ service, item, status, connected, onSave, onReset }: RowProps) {
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
    <div className="set-row">
      <span className="set-key">{item.key}</span>
      <span className="set-val">
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
          item.value || '—'
        )}
      </span>
      <span className="set-default">{item.default || '—'}</span>
      <span>
        {status?.state === 'pending' ? (
          <span className="tag-over">已覆盖 · 待确认</span>
        ) : status?.state === 'failed' ? (
          <span className="tag-over" title={status.error}>
            已覆盖 · 未送达
          </span>
        ) : item.overridden ? (
          <span className="tag-over">已覆盖</span>
        ) : (
          <span className="tag-default">默认</span>
        )}
      </span>
      <span className="set-actions">
        {editing ? (
          <>
            <button type="button" className="btn sm" onClick={() => void save()} disabled={busy || !connected}>
              保存
            </button>
            <button type="button" className="btn sm ghost" onClick={() => setEditing(false)} disabled={busy}>
              取消
            </button>
          </>
        ) : (
          <>
            <button type="button" className="btn sm" onClick={startEdit} disabled={busy || !connected}>
              修改
            </button>
            {item.overridden && (
              <button
                type="button"
                className={confirming ? 'btn sm danger' : 'btn sm quiet-danger'}
                onClick={() => void reset()}
                disabled={busy || !connected}
              >
                {confirming ? '确认重置' : '重置'}
              </button>
            )}
          </>
        )}
      </span>
    </div>
  );
}
