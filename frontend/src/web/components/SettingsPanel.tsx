// SettingsPanel 是"设置正文"：以**人**为单位呈现，而不是以键值为单位。
//  - 服务给的中文名/说明/控件类型（或语言包里的同名键）决定怎么画；
//  - 能改的和只读启动参数分两组，后者默认折叠，不占主视觉；
//  - 状态说人话：默认值 / 已改过 / 待服务确认 / 未送达 / 已设置；
//  - portalExtra：门户页在标题下注入自己的外观设置（本地偏好，不走服务设置协议）。
import { useState, type ReactNode } from 'react';
import type { RowStatus, SettingViewData, ServiceCardView } from '../types';
import type { Translate } from '../i18n';
import { IconCheck, IconClose, IconPencil, IconUndo } from './icons';

interface Props {
  service: ServiceCardView | null;
  settings: SettingViewData[] | null;
  /** 门户自己的外观设置：只在“门户”这个服务页里出现。 */
  portalExtra?: ReactNode;
  /** 名册里的服务名：给「服务名」这类设置项当候选（只是候选，校验权仍在服务）。 */
  peers: string[];
  connected: boolean;
  statusOf: (service: string, key: string) => RowStatus | undefined;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
  t: Translate;
}

export function SettingsPanel({ service, settings, portalExtra, peers, connected, statusOf, onSave, onReset, t }: Props) {
  if (!service) {
    return <p className="empty">{t('ui.settings.emptyAll', '还没有收到任何服务的设置声明')}</p>;
  }
  const title = t(`service.${service.name}.title`, service.title ?? service.name);
  const editable = (settings ?? []).filter((s) => !s.readonly);
  // 整段文本/JSON 这类"不是选项"的项收进高级：设置页的主角是开关与选项。
  const advanced = editable.filter((s) => s.kind === 'textarea');
  const simple = editable.filter((s) => s.kind !== 'textarea');
  const readOnly = (settings ?? []).filter((s) => s.readonly);

  return (
    <div className="panel">
      <h1 className="panel-title">{title}</h1>
      {portalExtra}
      <details className="ro-group about">
        <summary>{t('ui.settings.about', '关于这个服务')}</summary>
        {(() => {
          const desc = t(`service.${service.name}.desc`, service.description ?? '');
          return desc !== '' ? (
            <p className="panel-desc">{desc}</p>
          ) : (
            <p className="panel-desc">{t('ui.settings.noDesc', '这个服务没有留下说明')}</p>
          );
        })()}
        <p className="panel-addr">
          {t('ui.settings.address', '内部地址')}：{service.addr}
        </p>
      </details>

      {settings === null ? (
        <p className="empty">{t('ui.settings.loading', '加载中…')}</p>
      ) : (
        <>
          <h2 className="set-label">{t('ui.settings.editable', '可以改的')}</h2>
          {editable.length === 0 ? (
            <p className="empty">{t('ui.settings.empty', '这个服务没有可改的设置项')}</p>
          ) : simple.length === 0 ? (
            <p className="empty">{t('ui.settings.onlyAdvanced', '这个服务没有开关式的设置，只有下面这些高级项')}</p>
          ) : (
            <div className="form">
              {simple.map((item) => (
                <SettingRow
                  key={item.key}
                  service={service.name}
                  item={item}
                  peers={peers}
                  status={statusOf(service.name, item.key)}
                  connected={connected}
                  onSave={onSave}
                  onReset={onReset}
                  t={t}
                />
              ))}
            </div>
          )}

          {advanced.length > 0 && (
            <details className="ro-group">
              <summary>{t('ui.settings.advanced', '高级')}</summary>
              <p className="ro-hint">{t('ui.settings.advancedHint', '整段配置，改错会让服务拒绝这个值')}</p>
              <div className="form">
                {advanced.map((item) => (
                  <SettingRow
                    key={item.key}
                    service={service.name}
                    item={item}
                    peers={peers}
                    status={statusOf(service.name, item.key)}
                    connected={connected}
                    onSave={onSave}
                    onReset={onReset}
                    t={t}
                  />
                ))}
              </div>
            </details>
          )}

          {readOnly.length > 0 && (
            <details className="ro-group">
              <summary>{t('ui.settings.readonly', '启动参数')}</summary>
              <p className="ro-hint">{t('ui.settings.readonlyHint', '启动参数，改它要重启这个服务')}</p>
              <dl className="ro-list">
                {readOnly.map((it) => (
                  <div className="ro-row" key={it.key}>
                    <dt>
                      {t(
                        `setting.${service.name}.${it.key}.label`,
                        t(`setting.common.${it.key}`, it.label ?? it.key),
                      )}
                    </dt>
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
  /** 名册里的服务名（给"服务名"类设置项当候选）。 */
  peers: string[];
  status: RowStatus | undefined;
  connected: boolean;
  onSave: (service: string, key: string, value: string) => Promise<void>;
  onReset: (service: string, key: string) => Promise<void>;
  t: Translate;
}

// 单位符号 → 人话（服务里声明的是 ms / MB / KB / chars / count）。
const UNIT_LABELS: Record<string, string> = { ms: '毫秒', MB: 'MB', KB: 'KB', chars: '字', count: '个' };
function unitText(unit: string, t: Translate): string {
  return t(`ui.unit.${unit}`, UNIT_LABELS[unit] ?? unit);
}

// 数字项的"读法"：只换算展示，值本身不动（服务收到的仍是原始数值）。
function readout(unit: string, raw: string, t: Translate): string | null {
  const n = Number(raw);
  if (unit !== 'ms' || raw === '' || !Number.isFinite(n) || n <= 0) return null;
  const about = t('ui.readout.about', '≈');
  const round = (v: number): string => String(Math.round(v * 10) / 10);
  if (n >= 3600000) return `${about} ${round(n / 3600000)} ${t('ui.unit.hours', '小时')}`;
  if (n >= 60000) return `${about} ${round(n / 60000)} ${t('ui.unit.minutes', '分钟')}`;
  if (n >= 1000) return `${about} ${round(n / 1000)} ${t('ui.unit.seconds', '秒')}`;
  return null;
}

// SettingRow 是一个可改项：中文名 + 人话状态 + 合适的控件 + 一句说明。
// 开关和选项点一下即提交（不用先进编辑态）；只有文本/数字才需要输入。
function SettingRow({ service, item, peers, status, connected, onSave, onReset, t }: RowProps) {
  const [editing, setEditing] = useState(false);
  const [draft, setDraft] = useState('');
  const [busy, setBusy] = useState(false);
  const [confirming, setConfirming] = useState(false);

  const label = t(`setting.${service}.${item.key}.label`, item.label ?? item.key);
  const hint = t(`setting.${service}.${item.key}.hint`, item.hint ?? '');
  const kind = item.kind ?? (item.secret ? 'secret' : 'text');
  const unit = item.unit ?? '';
  const isSecret = item.secret === true;
  const clearLabel = isSecret ? t('ui.settings.action.clear', '清空') : t('ui.settings.action.reset', '恢复默认');
  const optionLabel = (value: string): string => t(`setting.${service}.${item.key}.opt.${value}`, value);
  const candidates = peers.filter((n) => n !== service);
  const disabled = busy || !connected;

  // commit 是一次提交；closeEdit 为真的路径（输入框）成功后回到展示态。
  const commit = async (value: string, closeEdit: boolean): Promise<void> => {
    if (value === item.value) {
      if (closeEdit) setEditing(false);
      return;
    }
    setBusy(true);
    try {
      await onSave(service, item.key, value);
      if (closeEdit) setEditing(false);
    } catch {
      // 失败提示由上层 toast 负责；不退出编辑态，用户可以改完再试
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
  const numReadout = kind === 'number' ? readout(unit, item.value, t) : null;

  const head = (
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
        {/* 秘密项没有"默认值"可回退：有值时才给清空（同样是两步确认）。 */}
        {!editing && (isSecret ? item.set === true : item.overridden) && (
          <button
            type="button"
            className="field-reset"
            data-armed={confirming ? 'true' : undefined}
            aria-label={
              confirming
                ? isSecret
                  ? t('ui.settings.action.confirmClear', '再点一次确认清空')
                  : t('ui.settings.action.confirmReset', '再点一次确认恢复默认')
                : clearLabel
            }
            title={
              confirming
                ? isSecret
                  ? t('ui.settings.action.confirmClear', '再点一次确认清空')
                  : t('ui.settings.action.confirmReset', '再点一次确认恢复默认')
                : clearLabel
            }
            onClick={() => void reset()}
            disabled={disabled}
          >
            <IconUndo />
          </button>
        )}
      </span>
    </div>
  );

  const saveButton = (onClick: () => void, extraDisabled = false) => (
    <button
      type="button"
      className="btn sm icon"
      aria-label={t('ui.settings.action.save', '保存')}
      aria-busy={busy}
      title={t('ui.settings.action.save', '保存')}
      onClick={onClick}
      disabled={disabled || extraDisabled}
    >
      <IconCheck />
    </button>
  );

  const cancelButton = (
    <button
      type="button"
      className="btn sm ghost icon"
      aria-label={t('ui.settings.action.cancel', '取消')}
      title={t('ui.settings.action.cancel', '取消')}
      onClick={() => {
        setEditing(false);
      }}
      disabled={busy}
    >
      <IconClose />
    </button>
  );

  const serviceSelect = (value: string, onChange: (next: string) => void) => (
    <select
      className="input"
      aria-label={label}
      value={value}
      disabled={disabled}
      onChange={(e) => {
        onChange(e.target.value);
      }}
    >
      {candidates.includes(value) ? null : (
        <option value={value}>{value === '' ? t('ui.settings.unset', '未设置') : value}</option>
      )}
      {candidates.map((n) => (
        <option key={n} value={n}>
          {n}
        </option>
      ))}
    </select>
  );

  // 开关：布尔项，或"可以没有"的服务项（关掉 = 清空）。
  if (kind === 'bool' || kind === 'optional-service') {
    const isBool = kind === 'bool';
    const on = isBool ? item.value === 'true' || item.value === '1' : item.value !== '';
    // 打开可空服务项时给个起点：声明的预设优先，其次名册里第一个候选。
    const fallback = item.default !== '' ? item.default : (candidates[0] ?? '');
    return (
      <div className="field">
        {head}
        <div className="field-control">
          <button
            type="button"
            className="switch"
            role="switch"
            aria-checked={on}
            aria-label={label}
            disabled={disabled}
            onClick={() => {
              void commit(isBool ? (on ? 'false' : 'true') : on ? '' : fallback, false);
            }}
          >
            <span className="switch-knob" />
          </button>
          {!isBool && on && serviceSelect(item.value, (next) => void commit(next, false))}
        </div>
        {hint !== '' && <p className="field-hint">{hint}</p>}
      </div>
    );
  }

  // 选项：少则分段，多则下拉；点一下即提交。
  if (kind === 'enum' && (item.options ?? []).length > 0) {
    const options = item.options ?? [];
    return (
      <div className="field">
        {head}
        <div className="field-control">
          {options.length <= 3 ? (
            <div className="seg" role="radiogroup" aria-label={label}>
              {options.map((o) => (
                <button
                  key={o}
                  type="button"
                  role="radio"
                  aria-checked={item.value === o}
                  className="seg-item"
                  disabled={disabled}
                  onClick={() => {
                    void commit(o, false);
                  }}
                >
                  {optionLabel(o)}
                </button>
              ))}
            </div>
          ) : (
            <select
              className="input"
              aria-label={label}
              value={item.value}
              disabled={disabled}
              onChange={(e) => {
                void commit(e.target.value, false);
              }}
            >
              {options.includes(item.value) ? null : <option value={item.value}>{optionLabel(item.value)}</option>}
              {options.map((o) => (
                <option key={o} value={o}>
                  {optionLabel(o)}
                </option>
              ))}
            </select>
          )}
        </div>
        {hint !== '' && <p className="field-hint">{hint}</p>}
      </div>
    );
  }

  // 服务名：候选来自名册，选完即提交（服务仍会自己校验）。
  if (kind === 'service') {
    return (
      <div className="field">
        {head}
        <div className="field-control">{serviceSelect(item.value, (next) => void commit(next, false))}</div>
        {hint !== '' && <p className="field-hint">{hint}</p>}
      </div>
    );
  }

  // 数字：带单位与读法；点铅笔才进输入态。
  if (kind === 'number') {
    return (
      <div className="field">
        {head}
        <div className="field-control">
          {editing ? (
            <>
              <input
                className="input"
                type="number"
                inputMode="numeric"
                autoFocus
                value={draft}
                aria-label={`${label} ${t('ui.settings.action.edit', '修改')}`}
                onChange={(e) => {
                  setDraft(e.target.value);
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void commit(draft, true);
                  if (e.key === 'Escape') {
                    // 同上：只退出本行编辑，不关闭设置页。
                    e.stopPropagation();
                    setEditing(false);
                  }
                }}
              />
              {unit !== '' && <span className="field-unit">{unitText(unit, t)}</span>}
              {saveButton(() => void commit(draft, true))}
              {cancelButton}
            </>
          ) : (
            <>
              <span className="field-text">
                <span className="field-value">{item.value === '' ? '—' : item.value}</span>
                {unit !== '' && <span className="field-unit">{unitText(unit, t)}</span>}
                {numReadout !== null && <span className="field-readout">{numReadout}</span>}
              </span>
              <button
                type="button"
                className="btn sm ghost icon"
                aria-label={t('ui.settings.action.edit', '修改')}
                title={t('ui.settings.action.edit', '修改')}
                onClick={startEdit}
                disabled={disabled}
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

  // 文本 / 秘密 / 长文本：需要输入；长文本（高级里的 textarea）带保存按钮。
  return (
    <div className="field">
      {head}
      <div className="field-control">
        {editing ? (
          <>
            {kind === 'textarea' ? (
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
                type={isSecret ? 'password' : 'text'}
                value={draft}
                aria-label={`${label} ${t('ui.settings.action.edit', '修改')}`}
                onChange={(e) => {
                  setDraft(e.target.value);
                }}
                onKeyDown={(e) => {
                  if (e.key === 'Enter') void commit(draft, true);
                  if (e.key === 'Escape') {
                    // 只退出本行编辑：别让同一次按键冒到 window，把整个设置页也关掉。
                    e.stopPropagation();
                    setEditing(false);
                  }
                }}
                autoFocus
              />
            )}
            {saveButton(() => void commit(draft, true), isSecret && draft === '')}
            {cancelButton}
          </>
        ) : (
          <>
            <span className="field-text">
              <span className="field-value">
                {isSecret ? (item.set === true ? '••••••••' : '—') : item.value === '' ? '—' : item.value}
              </span>
            </span>
            <button
              type="button"
              className="btn sm ghost icon"
              aria-label={t('ui.settings.action.edit', '修改')}
              title={t('ui.settings.action.edit', '修改')}
              onClick={startEdit}
              disabled={disabled}
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