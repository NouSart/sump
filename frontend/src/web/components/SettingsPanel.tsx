// SettingsPanel 是"设置正文"：以**人**为单位呈现，而不是以键值为单位。
//  - 服务给的中文名/说明/控件类型（或语言包里的同名键）决定怎么画；
//  - 能改的和只读启动参数分两组，后者默认折叠，不占主视觉；
//  - 状态说人话：默认值 / 已改过 / 待服务确认 / 未送达 / 已设置。
import { useState } from 'react';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';
import type { Translate } from '../i18n';
import { IconCheck, IconClose, IconPencil, IconUndo } from './icons';

interface Props {
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  connected: boolean;
  statusOf: (service: string, key: string) => RowStatus | undefined;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
  t: Translate;
}

export function SettingsPanel({ service, settings, connected, statusOf, onSave, onReset, t }: Props) {
  if (!service) {
    return <p className="empty">{t('ui.settings.emptyAll', '还没有收到任何服务的设置声明')}</p>;
  }
  const title = t(`service.${service.name}.title`, service.title ?? service.name);
  const editable = (settings ?? []).filter((s) => !s.readonly);
  const readOnly = (settings ?? []).filter((s) => s.readonly);

  return (
    <div className="panel">
      <h1 className="panel-title">{title}</h1>
      {(() => {
        const desc = t(`service.${service.name}.desc`, service.description ?? '');
        return desc !== '' ? <p className="panel-desc">{desc}</p> : null;
      })()}

      {settings === null ? (
        <p className="empty">{t('ui.settings.loading', '加载中…')}</p>
      ) : (
        <>
          <h2 className="set-label">{t('ui.settings.editable', '可以改的')}</h2>
          {editable.length === 0 ? (
            <p className="empty">{t('ui.settings.empty', '这个服务没有可改的设置项')}</p>
          ) : (
            <div className="form">
              {editable.map((item) => (
                <SettingRow
                  key={item.key}
                  service={service.name}
                  item={item}
                  status={statusOf(service.name, item.key)}
                  connected={connected}
                  onSave={onSave}
                  onReset={onReset}
                  t={t}
                />
              ))}
            </div>
          )}

          {readOnly.length > 0 && (
            <details className="ro-group">
              <summary>{t('ui.settings.readonly', '启动参数')}</summary>
              <p className="ro-hint">{t('ui.settings.readonlyHint', '启动参数，改它要重启这个服务')}</p>
              <dl className="ro-list">
                {readOnly.map((it) => (
                  <div className="ro-row" key={it.key}>
                    <dt>{t(`setting.${service.name}.${it.key}.label`, it.label ?? it.key)}</dt>
                    <dd title={it.value}>{it.value === '' ? '—' : it.value}</dd>
                  </div>
                ))}
              </dl>
            </details>
          )}
        </>
      )}
    </div>
  );
}

interface RowProps {
  service: string;
  item: SettingViewData;
  status: RowStatus | undefined;
  connected: boolean;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
  t: Translate;
}

// SettingRow 是一个可改项：中文名 + 人话状态 + 合适的控件 + 一句说明。
function SettingRow({ service, item, status, connected, onSave, onReset, t }: RowProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  const label = t(`setting.${service}.${item.key}.label`, item.label ?? item.key);
  const hint = t(`setting.${service}.${item.key}.hint`, item.hint ?? '');
  const kind = item.kind ?? (item.secret ? 'secret' : 'text');
  const isSecret = item.secret === true;
  const clearLabel = isSecret ? t('ui.settings.action.clear', '清空') : t('ui.settings.action.reset', '恢复默认');

  const save = async (value: string): Promise<void> => {
    // 没改动：只是退出编辑，不必打搅服务（也避免把同一个值再下发一遍）
    if (value === item.value) {
      setEditing(false);
      return;
    }
    setBusy(true);
    try {
      await onSave(service, item.key, value);
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
      setTimeout(() => {
        setConfirming(false);
      }, 3000);
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

  const startEdit = (): void => {
    setDraft(isSecret ? '' : item.value);
    setEditing(true);
  };

  const state = (): { text: string; cls: string; title?: string } => {
    if (status?.state === 'pending') return { text: t('ui.settings.state.pending', '待服务确认'), cls: 'tag-over' };
    if (status?.state === 'failed') {
      return { text: t('ui.settings.state.failed', '未送达'), cls: 'tag-over', title: status.error };
    }
    if (isSecret) {
      return item.set === true
        ? { text: t('ui.settings.state.set', '已设置'), cls: 'tag-over' }
        : { text: t('ui.settings.state.unset', '还没设置'), cls: 'tag-default' };
    }
    if (item.overridden) return { text: t('ui.settings.state.overridden', '已改过'), cls: 'tag-over' };
    return { text: t('ui.settings.state.default', '默认值'), cls: 'tag-default' };
  };
  const st = state();
  const drift = item.reported === true && item.actual !== item.value;

  // 布尔项：一个开关，点一下就是一次提交（不用先"编辑"）。
  if (kind === 'bool') {
    const on = item.value === 'true' || item.value === '1';
    return (
      <div className="field">
        <div className="field-head">
          <span className="field-label">{label}</span>
          <span className="field-badges">
            <span className={st.cls} title={st.title}>
              {st.text}
            </span>
            {drift && <span className="tag-drift">{t('ui.settings.state.drift', '服务侧已变')}</span>}
          </span>
        </div>
        <button
          type="button"
          className="switch"
          role="switch"
          aria-checked={on}
          aria-label={label}
          disabled={busy || !connected}
          onClick={() => void save(on ? 'false' : 'true')}
        >
          <span className="switch-knob" />
        </button>
        {hint !== '' && <p className="field-hint">{hint}</p>}
      </div>
    );
  }

  return (
    <div className="field">
      <div className="field-head">
        <span className="field-label">{label}</span>
        <span className="field-badges">
          <span className={st.cls} title={st.title}>
            {st.text}
          </span>
          {drift && (
            <span className="tag-drift" title={`${t('ui.settings.actual', '服务侧现值')}：${item.actual ?? ''}`}>
              {t('ui.settings.state.drift', '服务侧已变')}
            </span>
          )}
          {!editing && item.overridden && (
            <button
              type="button"
              className="field-reset"
              data-armed={confirming ? 'true' : undefined}
              aria-label={confirming ? t('ui.settings.action.confirmReset', '再点一次确认恢复默认') : clearLabel}
              title={confirming ? t('ui.settings.action.confirmReset', '再点一次确认恢复默认') : clearLabel}
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
            {kind === 'enum' ? (
              <select
                className="input"
                aria-label={`${label} ${t('ui.settings.action.edit', '修改')}`}
                value={draft}
                onChange={(e) => {
                  setDraft(e.target.value);
                }}
              >
                {(item.options ?? []).map((o) => (
                  <option key={o} value={o}>
                    {o}
                  </option>
                ))}
              </select>
            ) : kind === 'textarea' ? (
              <textarea
                className="input textarea"
                aria-label={`${label} ${t('ui.settings.action.edit', '修改')}`}
                rows={4}
                value={draft}
                onChange={(e) => {
                  setDraft(e.target.value);
                }}
              />
            ) : (
              <input
                className="input"
                type={isSecret ? 'password' : kind === 'number' ? 'number' : 'text'}
                value={draft}
                aria-label={`${label} ${t('ui.settings.action.edit', '修改')}`}
                onChange={(e) => {
                  setDraft(e.target.value);
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void save(draft);
                  if (e.key === 'Escape') setEditing(false);
                }}
                autoFocus
              />
            )}
            {kind === 'textarea' && (
              <button
                type="button"
                className="btn sm icon"
                aria-label={t('ui.settings.action.save', '保存')}
                title={t('ui.settings.action.save', '保存')}
                onClick={() => void save(draft)}
                disabled={busy || !connected}
              >
                <IconCheck />
              </button>
            )}
            {kind !== 'textarea' && (
              <button
                type="button"
                className="btn sm icon"
                aria-label={t('ui.settings.action.save', '保存')}
                aria-busy={busy}
                title={t('ui.settings.action.save', '保存')}
                onClick={() => void save(draft)}
                disabled={busy || !connected || (isSecret && draft === '')}
              >
                <IconCheck />
              </button>
            )}
            <button
              type="button"
              className="btn sm ghost icon"
              aria-label={t('ui.settings.action.cancel', '取消')}
              title={t('ui.settings.action.cancel', '取消')}
              onClick={() => setEditing(false)}
              disabled={busy}
            >
              <IconClose />
            </button>
          </>
        ) : (
          <>
            <span className="field-value">
              {isSecret ? (item.set === true ? '••••••••' : '—') : item.value === '' ? '—' : item.value}
            </span>
            <button
              type="button"
              className="btn sm ghost icon"
              aria-label={t('ui.settings.action.edit', '修改')}
              title={t('ui.settings.action.edit', '修改')}
              onClick={startEdit}
              disabled={busy || !connected}
            >
              <IconPencil />
            </button>
          </>
        )}
      </div>

      {hint !== '' && <p className="field-hint">{hint}</p>}
    </div>
  );
}