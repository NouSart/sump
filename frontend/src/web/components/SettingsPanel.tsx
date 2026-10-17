import { useState } from 'react';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';
import { IconCheck, IconClose, IconPencil, IconUndo } from './icons';

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
        <div className="form">
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

// SECRET_MASK 是秘密项的展示占位：值在中心那边就是空的，面板无从回显。
const SECRET_MASK = '••••••••';

function SettingRow({ service, item, status, connected, onSave, onReset }: RowProps) {
  const isSecret = item.secret === true;
  const isReadOnly = item.readonly === true;
  const clearLabel = isSecret ? '清空' : '重置';
  // 服务侧自己变了：它回传过的实际值与中心口径的生效值不一致（秘密项永远不比自己那一列）
  const drift = !isSecret && item.reported === true && (item.actual ?? '') !== item.value;
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
    // 秘密项不给值就不动它：清空走「清空」链（那里推的是空值）
    if (draft === item.value || (isSecret && draft === '')) {
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
    <div className="field">
      <div className="field-head">
        <span className="field-label">{item.key}</span>
        <span className="field-badges">
          {status?.state === 'pending' ? (
            <span className="tag-over">已覆盖 · 待确认</span>
          ) : status?.state === 'failed' ? (
            <span className="tag-over" title={status.error}>
              已覆盖 · 未送达
            </span>
          ) : isReadOnly ? (
            <span className="tag-default" title="启动参数带起来的，改它需要重启这个服务">
              不可改
            </span>
          ) : isSecret ? (
            item.set ? (
              <span className="tag-over">已设置</span>
            ) : (
              <span className="tag-default">未设置</span>
            )
          ) : item.overridden ? (
            <span className="tag-over">已覆盖</span>
          ) : (
            <span className="tag-default">默认</span>
          )}
          {drift && (
            <span className="tag-drift" title={`服务侧现值：${item.actual ?? ''}`}>
              服务侧已变
            </span>
          )}
          {!isReadOnly && (isSecret ? item.set : item.overridden) && !editing && (
            <button
              type="button"
              className="field-reset"
              data-armed={confirming ? 'true' : undefined}
              aria-label={confirming ? `确认${clearLabel}` : clearLabel}
              title={confirming ? `确认${clearLabel}` : clearLabel}
              onClick={() => void reset()}
              disabled={busy || !connected}
            >
              <IconUndo />
            </button>
          )}
        </span>
      </div>

      <div className="field-control">
        {editing ? (
          <>
            <input
              className="input"
              type={isSecret ? 'password' : 'text'}
              value={draft}
              aria-label={`${item.key} 的新值`}
              onChange={(e) => setDraft(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Enter') void save();
                if (e.key === 'Escape') setEditing(false);
              }}
              autoFocus
            />
            <button
              type="button"
              className="btn sm icon"
              aria-label="保存"
              aria-busy={busy}
              title="保存"
              onClick={() => void save()}
              disabled={busy || !connected || (isSecret && draft === '')}
            >
              <IconCheck />
            </button>
            <button
              type="button"
              className="btn sm ghost icon"
              aria-label="取消"
              title="取消"
              onClick={() => setEditing(false)}
              disabled={busy}
            >
              <IconClose />
            </button>
          </>
        ) : (
          <>
            <span className="field-value">{isSecret ? (item.set ? SECRET_MASK : '—') : item.value || '—'}</span>
            {!isReadOnly && (
              <button
                type="button"
                className="btn sm ghost icon"
                aria-label={isSecret ? (item.set ? '覆盖' : '设置') : '修改'}
                title={isSecret ? (item.set ? '覆盖' : '设置') : '修改'}
                onClick={startEdit}
                disabled={busy || !connected}
              >
                <IconPencil />
              </button>
            )}
          </>
        )}
      </div>

      <p className="field-hint">
        {isReadOnly
          ? '启动参数带起来的只读值——要改得改启动参数并重启这个服务'
          : isSecret
            ? '值由服务自己保存，中心不留也不回显'
            : `默认 ${item.default || '—'}`}
        {drift && <span> · 服务侧 {item.actual === '' ? '（空）' : item.actual}</span>}
      </p>
    </div>
  );
}
