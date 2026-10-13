import { useState } from 'react';
import type { SettingViewData, ServiceCardView } from '../types';

interface Props {
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  connected: boolean;
  revision?: number | null;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
}

function provideTitle(input?: string, output?: string): string {
  return [input ? `输入：${input}` : '', output ? `输出：${output}` : ''].filter(Boolean).join(' / ');
}

export function SettingsPanel({ service, settings, connected, revision, onSave, onReset }: Props) {
  if (!service) {
    return <div className="empty">尚未收到名册——等待设置中心广播，或检查面板与中心的连接。</div>;
  }
  return (
    <div className="panel">
      <header className="panel-head">
        <p className="eyebrow">
          服务
          {revision !== null && revision !== undefined && <span className="eyebrow-meta">REV {revision}</span>}
        </p>
        <h2 className="panel-title">{service.name}</h2>
        {service.description && <p className="panel-desc">{service.description}</p>}
        <dl className="meta">
          <div className="meta-row">
            <dt className="meta-label">地址</dt>
            <dd className="meta-value">{service.addr}</dd>
          </div>
          {(service.provides?.length ?? 0) > 0 && (
            <div className="meta-row">
              <dt className="meta-label">动作</dt>
              <dd className="meta-value">
                {service.provides?.map((p) => (
                  <span key={p.action} className={p.tool ? 'tag tag-tool' : 'tag'} title={provideTitle(p.input, p.output)}>
                    {p.action}
                    {p.tool && <span className="tag-mark">工具</span>}
                  </span>
                ))}
              </dd>
            </div>
          )}
        </dl>
      </header>

      <div className="settings-head">
        <p className="section-label">设置项</p>
      </div>
      {settings === null ? (
        <p className="empty">加载中…</p>
      ) : settings.length === 0 ? (
        <p className="empty">该服务未声明可设置项——面板只显示服务真实声明并消费的设置（不做假旋钮）。</p>
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
    <div className="set-row">
      <div className="set-key">{item.key}</div>
      <div className="set-val">
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
          (item.value || '—')
        )}
      </div>
      <div className="set-default">{item.default || '—'}</div>
      <div>{item.overridden ? <span className="tag-over">已覆盖</span> : <span className="tag-default">默认</span>}</div>
      <div className="set-actions">
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
                className={confirming ? 'btn danger' : 'btn quiet-danger'}
                onClick={() => void reset()}
                disabled={busy || !connected}
              >
                {confirming ? '确认重置' : '重置'}
              </button>
            )}
          </>
        )}
      </div>
    </div>
  );
}
