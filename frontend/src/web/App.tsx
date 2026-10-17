import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import type { CSSProperties } from 'react';
import { fetchOverview, resetSetting, sendMessage, setSetting } from './api';
import { PortalView } from './components/PortalView';
import { ServiceList } from './components/ServiceList';
import { SettingsPanel } from './components/SettingsPanel';
import { Toasts, type Toast } from './components/Toasts';
import { IconDark, IconLight, IconSettings } from './components/icons';
import type {
  Overview,
  PortalMessage,
  RosterView,
  RowStatus,
  ServiceSettingsView,
  SettingsEventViewData,
  TimerView,
} from './types';

let toastSeq = 0;

// 外观：DSH 的暗色是令牌级的 body[data-ds-dark-theme]，我们只切这一个属性，
// 颜色全部由 --dsw-* 自动跟随，不另画一套色。首选项存本地，没有就用系统偏好。
type Theme = 'light' | 'dark';
const ThemeKey = 'sump.portal.theme';

function initialTheme(): Theme {
  const saved = window.localStorage.getItem(ThemeKey);
  if (saved === 'light' || saved === 'dark') return saved;
  // jsdom 没有 matchMedia，没有就按浅色
  const prefersDark =
    typeof window.matchMedia === 'function' && window.matchMedia('(prefers-color-scheme: dark)').matches;
  return prefersDark ? 'dark' : 'light';
}

export function App() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [roster, setRoster] = useState<RosterView | null>(null);
  const [connected, setConnected] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [theme, setTheme] = useState<Theme>(initialTheme);

  useEffect(() => {
    if (theme === 'dark') document.body.dataset.dsDarkTheme = '';
    else delete document.body.dataset.dsDarkTheme;
    window.localStorage.setItem(ThemeKey, theme);
  }, [theme]);
  const [rowStates, setRowStates] = useState<Record<string, RowStatus>>({});
  // 视图：门户（默认，主体）与设置（从侧栏底部进入的独立页）。
  const [view, setView] = useState<'portal' | 'settings'>('portal');
  const [messages, setMessages] = useState<PortalMessage[]>([]);
  const [timers, setTimers] = useState<TimerView[]>([]);
  const [portalError, setPortalError] = useState<string | null>(null);
  const pendingKeysRef = useRef(new Set<string>());
  const rosterRevRef = useRef(-1);

  const pushToast = useCallback((kind: Toast['kind'], text: string) => {
    const id = ++toastSeq;
    setToasts((t) => [...t, { id, kind, text }]);
    setTimeout(() => setToasts((t) => t.filter((x) => x.id !== id)), 4200);
  }, []);

  // applyRoster 应用名册：只接受"不更旧"的修订，返回是否发生了修订变化。
  // 首屏 fetch 与 SSE 是两条独立路径，旧响应可能后到——无条件覆盖会让界面回退到旧名册
  // （并且把守卫水位一起倒退）。相同修订重复应用是幂等的，放行。
  const applyRoster = useCallback((r: RosterView): boolean => {
    if (r.revision < rosterRevRef.current) return false;
    const changed = r.revision !== rosterRevRef.current;
    rosterRevRef.current = r.revision;
    setRoster(r);
    return changed;
  }, []);

  const load = useCallback(async () => {
    try {
      const o = await fetchOverview();
      setOverview(o);
      setConnected(o.connected);
      if (o.roster) applyRoster(o.roster);
      setLoadError(null);
    } catch (err) {
      setLoadError((err as Error).message);
    }
  }, [applyRoster]);

  const loadRef = useRef(load);
  useEffect(() => {
    loadRef.current = load;
  }, [load]);
  useEffect(() => {
    void load();
  }, [load]);

  // settings 事件：更新行的会话内状态（待确认 → 已生效 / 未送达），并刷新设置视图。
  const handleSettingsEvent = useCallback(
    (e: SettingsEventViewData) => {
      const svc = e.service ?? '';
      // 服务自报（report_setting）不是"我们推下去的那一次"的回执：不能拿它去确认/否决用户刚提交的修改
      const results = e.cause === 'report_setting' ? [] : (e.results ?? []);
      for (const r of results) {
        const k = `${svc}.${r.key}`;
        const wasPending = pendingKeysRef.current.delete(k);
        if (r.ok === true) {
          setRowStates((prev) => {
            if (!(k in prev)) return prev;
            const next = { ...prev };
            delete next[k];
            return next;
          });
          if (wasPending) pushToast('ok', `已生效 ${k}${r.value ? ` = ${r.value}` : ''}`);
        } else {
          const reason = r.error ?? '未送达';
          setRowStates((prev) => ({ ...prev, [k]: { state: 'failed', error: reason } }));
          if (wasPending) pushToast('error', `未生效 ${k}：${reason}`);
        }
      }
      void loadRef.current();
    },
    [pushToast],
  );
  const settingsHandlerRef = useRef(handleSettingsEvent);
  useEffect(() => {
    settingsHandlerRef.current = handleSettingsEvent;
  }, [handleSettingsEvent]);

  // SSE：中心连接状态与名册实时更新；重连后的首次 connected 事件触发一次重载。
  const wasConnectedRef = useRef(false);
  useEffect(() => {
    const es = new EventSource('/api/events');
    es.addEventListener('status', (ev) => {
      const d = JSON.parse((ev as MessageEvent).data) as { connected: boolean };
      setConnected(d.connected);
      if (d.connected && !wasConnectedRef.current) {
        // 重连：中心可能重启过（revision 从头计）→ 松开修订守卫，让新名册能进来。
        rosterRevRef.current = -1;
        void loadRef.current();
      }
      wasConnectedRef.current = d.connected;
    });
    es.addEventListener('roster', (ev) => {
      const d = JSON.parse((ev as MessageEvent).data) as RosterView;
      // 名册修订变化（含服务重启重注册、声明变化）→ 重新拉取设置视图；
      // 更旧的修订（迟到事件）直接丢弃，不覆盖、也不触发重拉。
      if (applyRoster(d)) void loadRef.current();
    });
    es.addEventListener('settings', (ev) => {
      settingsHandlerRef.current(JSON.parse((ev as MessageEvent).data) as SettingsEventViewData);
    });
    // 门户投递：智能体经 deliver 送来的消息，落到对话里。
    es.addEventListener('deliver', (ev) => {
      const d = JSON.parse((ev as MessageEvent).data) as { text: string; at: string };
      setMessages((prev) => [...prev, { author: 'agent', text: d.text, at: d.at }]);
    });
    // 计时快照：timer 受理/到点/撤销后推来的**全量列表**（不是增量），直接覆盖。
    es.addEventListener('timers', (ev) => {
      setTimers(JSON.parse((ev as MessageEvent).data) as TimerView[]);
    });
    return () => es.close();
  }, []);

  const services = useMemo(() => roster?.services ?? [], [roster]);

  useEffect(() => {
    if (services.length === 0) {
      setSelected(null);
      return;
    }
    if (selected !== null && services.some((s) => s.name === selected)) return;
    setSelected(services[0]?.name ?? null);
  }, [services, selected]);

  const selectedService = useMemo(() => services.find((s) => s.name === selected) ?? null, [services, selected]);
  const selectedSettings = useMemo(() => {
    if (overview === null || selected === null) return null;
    return overview.settings?.services.find((s) => s.service === selected)?.settings ?? [];
  }, [overview, selected]);
  const svcIndex = useMemo(() => {
    if (selected === null) return 0;
    const i = services.findIndex((s) => s.name === selected);
    return i >= 0 ? i + 1 : 0;
  }, [services, selected]);

  const applyUpdates = useCallback((updated: ServiceSettingsView[]) => {
    setOverview((prev) => {
      if (!prev) return prev;
      const list = [...(prev.settings?.services ?? [])];
      for (const u of updated) {
        const idx = list.findIndex((s) => s.service === u.service);
        if (idx >= 0) list[idx] = u;
        else list.push(u);
      }
      return { ...prev, settings: { services: list } };
    });
  }, []);

  const onSave = useCallback(
    async (service: string, key: string, value: string) => {
      const k = `${service}.${key}`;
      pendingKeysRef.current.add(k);
      setRowStates((prev) => ({ ...prev, [k]: { state: 'pending' } }));
      try {
        const updated = await setSetting(service, key, value);
        applyUpdates(updated);
        pushToast('ok', `已写入 ${k}（待服务确认）`);
      } catch (err) {
        pendingKeysRef.current.delete(k);
        setRowStates((prev) => {
          const next = { ...prev };
          delete next[k];
          return next;
        });
        pushToast('error', `写入失败：${(err as Error).message}`);
        throw err;
      }
    },
    [applyUpdates, pushToast],
  );

  const onReset = useCallback(
    async (service: string, key: string) => {
      const k = `${service}.${key}`;
      pendingKeysRef.current.add(k);
      setRowStates((prev) => ({ ...prev, [k]: { state: 'pending' } }));
      try {
        const updated = await resetSetting(service, key);
        applyUpdates(updated);
        pushToast('ok', `已重置 ${k}（待服务确认）`);
      } catch (err) {
        pendingKeysRef.current.delete(k);
        setRowStates((prev) => {
          const next = { ...prev };
          delete next[k];
          return next;
        });
        pushToast('error', `重置失败：${(err as Error).message}`);
        throw err;
      }
    },
    [applyUpdates, pushToast],
  );

  const settingsCountOf = useCallback(
    (name: string) => overview?.settings?.services.find((s) => s.service === name)?.settings?.length ?? 0,
    [overview],
  );

  const onSend = useCallback(async (text: string) => {
    setPortalError(null);
    // 本地先回显（受理回执只说明消息已交给 reasoner，不代表已有回复）。
    setMessages((prev) => [...prev, { author: 'user', text, at: new Date().toISOString() }]);
    try {
      await sendMessage(text);
    } catch (err) {
      setPortalError(`未送达：${(err as Error).message}`);
    }
  }, []);

  return (
    <div className="app">
      <aside className="sidebar">
        <div className="brand">
          SUMP
          <span className="brand-sub">门户</span>
        </div>
        <div className="side-status">
          <span
            className={connected ? 'status status-on' : 'status status-off'}
            title={overview ? `设置中心 ${overview.center}` : '设置中心'}
          >
            <span className="status-dot" aria-hidden="true" />
            {connected ? '已连接' : '未连接'}
          </span>
        </div>
        {/* 侧栏底部：外观开关 + 设置入口（设置入口与 DSH 的设置页同一位置）。 */}
        <div className="side-foot">
          <div
            className="theme-switch"
            role="radiogroup"
            aria-label="外观"
            style={{ '--dsh-segment-count': 2, '--dsh-segment-index': theme === 'dark' ? 1 : 0 } as CSSProperties}
          >
            <span className="theme-indicator" aria-hidden="true" />
            <button
              type="button"
              role="radio"
              aria-checked={theme === 'light'}
              className="theme-tab"
              onClick={() => {
                setTheme('light');
              }}
            >
              <IconLight />
              浅色
            </button>
            <button
              type="button"
              role="radio"
              aria-checked={theme === 'dark'}
              className="theme-tab"
              onClick={() => {
                setTheme('dark');
              }}
            >
              <IconDark />
              深色
            </button>
          </div>
          <button
            type="button"
            className="side-row"
            aria-current={view === 'settings' ? 'page' : undefined}
            onClick={() => {
              setView('settings');
            }}
          >
            <IconSettings />
            设置
          </button>
        </div>
      </aside>

      <main className="content">
        {loadError && (
          <p className="inline-error" role="alert">
            加载失败：{loadError}
          </p>
        )}

        {view === 'portal' ? (
          <PortalView connected={connected} messages={messages} timers={timers} error={portalError} onSend={onSend} />
        ) : (
          <section className="settings-page" aria-label="设置">
            <header className="page-head">
              <h1>设置</h1>
              <button
                type="button"
                className="page-close"
                onClick={() => {
                  setView('portal');
                }}
              >
                关闭
              </button>
            </header>
            <div className="settings-body">
              <nav aria-label="服务列表">
                <p className="section-label">服务 · {services.length}</p>
                <ServiceList services={services} selected={selected} onSelect={setSelected} settingsCountOf={settingsCountOf} />
              </nav>
              <div className="settings-content">
                <SettingsPanel
                  service={selectedService}
                  settings={selectedSettings}
                  connected={connected}
                  index={svcIndex}
                  total={services.length}
                  statusOf={(service, key) => rowStates[`${service}.${key}`]}
                  onSave={onSave}
                  onReset={onReset}
                />
              </div>
            </div>
          </section>
        )}
      </main>

      <Toasts items={toasts} />
    </div>
  );
}
