import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { decideApproval, fetchHistory, fetchOverview, resetSetting, sendMessage, setSetting } from './api';
import { PortalView } from './components/PortalView';
import { IconChevronUp } from './components/icons';
import { ServiceList } from './components/ServiceList';
import { SettingsView } from './components/SettingsView';
import { useLocalePack } from './i18n';
import { Toasts, type Toast } from './components/Toasts';
import type {
  Overview,
  PendingApprovalView,
  PortalMessage,
  RosterView,
  RowStatus,
  ServiceSettingsView,
  SettingsEventViewData,
  TimerView,
} from './types';

let toastSeq = 0;

// 外观：整站只有一套令牌，深/浅由 <html data-theme> 切换（见 styles.css）。首选项存本地，没有就跟系统。
type Theme = 'light' | 'dark';
const ThemeKey = 'sump.portal.theme';

// appendMessage 追加一条消息；与上一条完全同角色同文本时跳过——
// 和 memory 的 store 幂等规则一致，避免"历史回填 + 实时推送"把同一条画两遍。
function appendMessage(prev: PortalMessage[], m: PortalMessage): PortalMessage[] {
  const last = prev[prev.length - 1];
  if (last !== undefined && last.author === m.author && last.text === m.text) return prev;
  return [...prev, m];
}

// mergeMessages 把历史接在实时消息前面，并压掉交界处的相邻重复（同样只压相邻）。
function mergeMessages(history: PortalMessage[], live: PortalMessage[]): PortalMessage[] {
  let out: PortalMessage[] = [];
  for (const m of [...history, ...live]) out = appendMessage(out, m);
  return out;
}

function initialTheme(): Theme {
  const saved = window.localStorage.getItem(ThemeKey);
  if (saved === 'light' || saved === 'dark') return saved;
  // jsdom 没有 matchMedia，没有就按浅色
  const prefersDark =
    typeof window.matchMedia === 'function' && window.matchMedia('(prefers-color-scheme: dark)').matches;
  return prefersDark ? 'dark' : 'light';
}

export function App() {  const [overview, setOverview] = useState<Overview | null>(null);
  const [roster, setRoster] = useState<RosterView | null>(null);
  const [connected, setConnected] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [toasts, setToasts] = useState<Toast[]>([]);
  const [theme, setTheme] = useState<Theme>(initialTheme);

  useEffect(() => {
    document.documentElement.dataset.theme = theme;
    window.localStorage.setItem(ThemeKey, theme);
  }, [theme]);
  const [rowStates, setRowStates] = useState<Record<string, RowStatus>>({});
  // 视图：对话是主体；设置是右侧抽屉（从输入框的「设置」进入）。
  const [settingsOpen, setSettingsOpen] = useState(false);
  // 侧栏收起＝向上塌成一条（草图里的 ↑ 收起按钮），不是缩成竖轨：
  // 收起后字标留在顶上、对话区接管整幅宽度。
  const [collapsed, setCollapsed] = useState(false);
  // 语言包：界面文案全部走它，切语言即时生效（不重启服务）。
  const { locale, locales, setLocale, t } = useLocalePack();
  const [messages, setMessages] = useState<PortalMessage[]>([]);
  const [timers, setTimers] = useState<TimerView[]>([]);
  const [approvals, setApprovals] = useState<PendingApprovalView[]>([]);
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
      setMessages((prev) => appendMessage(prev, { author: 'agent', text: d.text, at: d.at }));
    });
    // 计时快照：timer 受理/到点/撤销后推来的**全量列表**（不是增量），直接覆盖。
    es.addEventListener('timers', (ev) => {
      setTimers(JSON.parse((ev as MessageEvent).data) as TimerView[]);
    });
    // 待审命令：approval 挂起后推来的**全量列表**（新到、已裁决、过期都在里面），直接覆盖。
    es.addEventListener('approvals', (ev) => {
      setApprovals(JSON.parse((ev as MessageEvent).data) as PendingApprovalView[]);
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

  // 会话侧栏的时间戳：取这条对话最后一条消息的时间（回归"最近说话"的直觉）；没有就留占位。
  const threadLabel = useMemo(() => {
    const last = messages[messages.length - 1];
    return last ? last.at.slice(11, 16) : '—:—';
  }, [messages]);

  // 历史回填：门户启动时向 memory 要最近的对话——真相在服务，门户只负责显示。
  // 失败只提示，不阻断：新消息照样能发、能收。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const history = await fetchHistory();
        if (cancelled || history.length === 0) return;
        setMessages((prev) => mergeMessages(history, prev));
      } catch (err) {
        if (!cancelled) setPortalError(`历史未取回：${(err as Error).message}`);
      }
    })();
    return () => {
      cancelled = true;
    };
  }, []);

  const onSend = useCallback(async (text: string) => {
    setPortalError(null);
    // 本地先回显（受理回执只说明消息已交给 reasoner，不代表已有回复）。
    setMessages((prev) => appendMessage(prev, { author: 'user', text, at: new Date().toISOString() }));
    try {
      await sendMessage(text);
    } catch (err) {
      setPortalError(`未送达：${(err as Error).message}`);
    }
  }, []);

  // onDecide：把用户的裁决送回 approval。成功后由 SSE 的 approvals 事件把卡片删掉
  // （服务端是唯一真相：门户不自己先删，避免"界面说批了、其实 approval 已经过期"）。
  const onDecide = useCallback(async (itemId: string, decision: 'allow' | 'deny') => {
    setPortalError(null);
    try {
      await decideApproval(itemId, decision);
    } catch (err) {
      setPortalError(`裁决未送达：${(err as Error).message}`);
    }
  }, []);

  return (
    <div className="app" data-collapsed={collapsed ? '1' : undefined}>
      <div className="aurora" aria-hidden="true">
        <span />
        <span />
        <span />
      </div>

      {/* 字标与连接点挂在壳上（见 styles.css 里定死的坐标）：侧栏展开/收起、
          设置抽屉、终端都动不了它们的位置。 */}
      <span className="brand">SUMP</span>
      {!collapsed && (
        <span
          className={connected ? 'dot dot-on' : 'dot dot-off'}
          role="img"
          aria-label={connected ? '已连接设置中心' : '未连接设置中心'}
          title={overview ? `设置中心 ${overview.center} · ${connected ? '已连接' : '未连接'}` : '设置中心'}
        />
      )}

      {collapsed ? (
        <button
          type="button"
          className="sidebar sidebar-bar"
          aria-expanded={false}
          aria-label="展开侧栏"
          title="展开侧栏"
          onClick={() => {
            setCollapsed(false);
          }}
        />
      ) : (
        <aside className="sidebar">
          <div className="brand-slot" aria-hidden="true" />
          {settingsOpen ? (
            <nav className="threads" aria-label="服务列表">
              <p className="section-label">{t('ui.settings.services', '服务')} · {services.length}</p>
              <ServiceList
                services={services}
                selected={selected}
                onSelect={setSelected}
                settingsCountOf={settingsCountOf}
                t={t}
              />
            </nav>
          ) : (
            <nav className="threads" aria-label="会话">
              {/* 目前 reasoner 只有单会话（web:private:owner）：列表如实只显示这一条，
                  不做假的多会话条目——等多会话能力落地再长出来。 */}
              <button type="button" className="thread is-active" aria-current="true" title="当前会话">
                <span className="t-time">{threadLabel}</span>
                <span className="t-title">当前会话</span>
              </button>
            </nav>
          )}
          <div className="me">
            <span className="avatar" aria-hidden="true">{t('ui.me.avatar', '我')}</span>
            <span className="me-name">{t('ui.me.name', '本机用户')}</span>
            <button
              type="button"
              className="icon-btn"
              aria-expanded={true}
              aria-label="收起侧栏"
              title="收起侧栏"
              onClick={() => {
                setCollapsed(true);
              }}
            >
              <IconChevronUp />
            </button>
          </div>
        </aside>
      )}

      <main className="stage">
        {loadError && (
          <p className="inline-error" role="alert">
            加载失败：{loadError}
          </p>
        )}
        {settingsOpen ? (
          <SettingsView
            theme={theme}
            onTheme={setTheme}
            onClose={() => {
              setSettingsOpen(false);
            }}
            service={selectedService}
            settings={selectedSettings}
            connected={connected}
            statusOf={(service, key) => rowStates[`${service}.${key}`]}
            onSave={onSave}
            onReset={onReset}
            locale={locale}
            locales={locales}
            onLocale={setLocale}
            t={t}
          />
        ) : (
          <PortalView
            connected={connected}
            messages={messages}
            timers={timers}
            approvals={approvals}
            onDecide={onDecide}
            error={portalError}
            onSend={onSend}
            onOpenSettings={() => {
              setSettingsOpen(true);
            }}
          />
        )}
      </main>

      <Toasts items={toasts} />
    </div>
  );
}
