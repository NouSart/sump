import { useCallback, useEffect, useMemo, useRef, useState } from 'react';
import { decideApproval, fetchHistory, fetchOverview, fetchTasks, parseMeta, resetSetting, resumeTask, sendMessage, setSetting } from './api';
import { PortalView } from './components/PortalView';
import { IconChevronUp } from './components/icons';
import { ServiceList } from './components/ServiceList';
import { TaskList } from './components/TaskList';
import { SettingsView } from './components/SettingsView';
import { useLocalePack } from './i18n';
import { Toasts, type Toast } from './components/Toasts';
import { useTimeTheme } from './timeTheme';
import { useWorkflowExpand } from './workflowExpand';
import type {
  Overview,
  PendingApprovalView,
  PortalMessage,
  ProgressEventView,
  RosterView,
  RowStatus,
  ServiceSettingsView,
  SettingsEventViewData,
  TaskView,
  TimerView,
} from './types';

let toastSeq = 0;
// 本地消息 id：没有 seq 的消息（本地回显 / deliver）用它做 React key。
// 用数组下标当 key 会在乱序插入时让后面的步骤重挂载、丢掉已展开的折叠状态。
let localMessageSeq = 0;
function nextLocalMessageID(): string {
  localMessageSeq++;
  return `local-${String(localMessageSeq)}`;
}

// appendMessage 追加一条消息：
//  - 有 seq（落库过的记录）就按 seq 去重与插入——实时推送与历史回填可能同时到，seq 是同一把尺子；
//  - 没有 seq（本地回显的用户消息、deliver 送来的答复）就按"上一条同角色同文本"跳过。
function appendMessage(prev: PortalMessage[], m: PortalMessage): PortalMessage[] {
  const seq = m.seq ?? 0;
  if (seq > 0) {
    if (prev.some((p) => p.seq === seq)) return prev;
    // 乱序到达（progress 是并发发出的）：插到第一个更大 seq 之前，而不是无脑追加。
    const at = prev.findIndex((p) => (p.seq ?? 0) > seq);
    if (at >= 0) return [...prev.slice(0, at), m, ...prev.slice(at)];
    return [...prev, m];
  }
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

export function App() {
  const [overview, setOverview] = useState<Overview | null>(null);
  const [roster, setRoster] = useState<RosterView | null>(null);
  const [connected, setConnected] = useState(false);
  const [selected, setSelected] = useState<string | null>(null);
  // 用户是否亲手点过服务项：点过就尊重选择；没点过则每次进设置都回门户。
  const selectedByUserRef = useRef(false);
  const [loadError, setLoadError] = useState<string | null>(null);
  const [toasts, setToasts] = useState<Toast[]>([]);
  // 整站配色（底色 / 光斑 / 强调色 / 深浅面板）跟随本地时间；设置页只保留跟随开关与时段滑块。
  const timeTheme = useTimeTheme();
  // 工作流展开：过程条目（思考 / 输出）什么时候自己摊开——本地偏好，与外观同一路数。
  const workflowExpand = useWorkflowExpand();
  const [rowStates, setRowStates] = useState<Record<string, RowStatus>>({});
  // 视图：对话是主体；设置是右侧抽屉（从输入框的「设置」进入）。
  const [settingsOpen, setSettingsOpen] = useState(false);
  // 侧栏收起＝向上塌成一条（草图里的 ↑ 收起按钮），不是缩成竖轨：
  // 收起后字标留在顶上、对话区接管整幅宽度。
  const [collapsed, setCollapsed] = useState(false);
  // 语言包：界面文案全部走它，切语言即时生效（不重启服务）。
  const { locale, locales, setLocale, t } = useLocalePack();
  const [messages, setMessages] = useState<PortalMessage[]>([]);
  const [tasks, setTasks] = useState<TaskView[]>([]);
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
          if (wasPending) pushToast('ok', `${t('ui.app.applied', '已生效')} ${k}${r.value ? ` = ${r.value}` : ''}`);
        } else {
          const reason = r.error ?? '未送达';
          setRowStates((prev) => ({ ...prev, [k]: { state: 'failed', error: reason } }));
          if (wasPending) pushToast('error', `${t('ui.app.notApplied', '未生效')} ${k}：${reason}`);
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
      const d = JSON.parse((ev as MessageEvent).data) as { text: string; taskId?: string; at: string };
      setMessages((prev) =>
        appendMessage(prev, {
          author: 'agent',
          kind: 'assistant',
          text: d.text,
          at: d.at,
          taskId: d.taskId ?? '',
          id: nextLocalMessageID(),
        }),
      );
      // 答复到了：任务名/状态可能刚被判定改过，顺手对齐一次（失败不影响对话）。
      void fetchTasks()
        .then(setTasks)
        .catch(() => undefined);
    });
    // 任务实况：思考 / 终端命令 / 终端输出——进行中就画出来，不必等最终答复。
    es.addEventListener('progress', (ev) => {
      const d = JSON.parse((ev as MessageEvent).data) as ProgressEventView;
      const kind = d.kind;
      if (kind !== 'reasoning' && kind !== 'tool_call' && kind !== 'tool_result') return;
      setMessages((prev) =>
        appendMessage(prev, {
          author: 'agent',
          kind,
          text: d.content,
          at: d.at,
          seq: d.seq,
          taskId: d.taskId,
          meta: parseMeta(d.meta),
        }),
      );
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

  const portalName = overview?.self.name ?? null;
  // 门户排在最上面：它承载自己的外观设置，也方便用户一进来就改主题。
  // 名册还没到（或中心断线）时，用 overview.self 补一条，保证门户页仍可进入。
  const services = useMemo(() => {
    const list = roster?.services ?? [];
    if (portalName === null) return list;
    const portal = list.find((s) => s.name === portalName);
    const rest = list.filter((s) => s.name !== portalName);
    return [portal ?? { name: portalName, addr: overview?.self.ws ?? '' }, ...rest];
  }, [roster, portalName, overview?.self.ws]);

  useEffect(() => {
    if (services.length === 0) {
      setSelected(null);
      return;
    }
    // 未手动选过时始终回门户；名册先到、overview 后到时也不会被“审批”占住默认页。
    setSelected((current) => {
      if (selectedByUserRef.current && current !== null && services.some((s) => s.name === current)) {
        return current;
      }
      const portal = portalName !== null && services.some((s) => s.name === portalName) ? portalName : null;
      return portal ?? services[0]?.name ?? null;
    });
  }, [services, portalName]);

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
        pushToast('error', `${t('ui.app.writeFailed', '写入失败：')}${(err as Error).message}`);
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

  // ---------- 任务 ----------
  // 面板按**会话**来：整段会话一次拉全（按任务分段），任务只是会话里的分节。
  // 侧栏的树是"会话 → 任务"，点任务 = 跳到那一段（不切换视图、不过滤）。
  const taskIdsWithRecords = useMemo(() => new Set(messages.map((m) => m.taskId ?? '')), [messages]);

  // 当前任务 = 最近活跃（updated_at 最大）的进行中任务，与 memory 的 CurrentTask 同判据。
  // 用"最早创建的 active"会在恢复了旧任务时续错任务。
  const currentTask = useMemo(() => {
    let best: TaskView | null = null;
    for (const task of tasks) {
      if (task.status !== 'active') continue;
      if (best === null || task.updated_at > best.updated_at) best = task;
    }
    return best;
  }, [tasks]);

  // 用"临时关掉平滑滚动 + 直接写 scrollTop"落位而不是 scrollIntoView：容器是 scroll-behavior:smooth，
  // 跨几千像素的平滑滚动又慢又容易被打断；跳转要的是"立刻到那"，再用一下高亮说明落在哪一段。
  const jumpToTask = useCallback((taskId: string) => {
    const el = document.getElementById(`task-${taskId}`);
    const list = el?.closest('.portal-list');
    if (el === null || !(list instanceof HTMLElement)) return false;
    const behavior = list.style.scrollBehavior;
    list.style.scrollBehavior = 'auto';
    list.scrollTop += el.getBoundingClientRect().top - list.getBoundingClientRect().top - 12;
    list.style.scrollBehavior = behavior;
    el.classList.remove('is-jump');
    // 强制回流：同一段连点两次也要重新播放高亮
    void el.offsetWidth;
    el.classList.add('is-jump');
    window.setTimeout(() => {
      el.classList.remove('is-jump');
    }, 1400);
    return true;
  }, []);

  // onJumpTask：侧栏点任务。已经在会话里就跳过去；不在就先把它这一段取回来——
  // 进行中的任务走 task_resume（记忆服务会把它顶成当前任务），已完结的直接按 task_id 读历史。
  const onJumpTask = useCallback(
    (taskId: string) => {
      if (jumpToTask(taskId)) return;
      const task = tasks.find((x) => x.id === taskId);
      if (task === undefined) return;
      setPortalError(null);
      const load = task.status === 'active' ? resumeTask(taskId) : fetchHistory(200, 0, taskId);
      void load
        .then((history) => {
          if (history.length === 0) {
            pushToast('error', `${task.name} · ${t('ui.task.noRecords', '没有记录可跳')}`);
            return;
          }
          setMessages((prev) => mergeMessages(history, prev));
          // 记录落进列表后才有分节头：等一帧再滚，否则拿不到刚渲染的元素。
          window.setTimeout(() => {
            jumpToTask(taskId);
          }, 0);
          if (task.status === 'active') {
            void fetchTasks()
              .then(setTasks)
              .catch(() => undefined);
          }
        })
        .catch((err: unknown) => {
          setPortalError(`${t('ui.app.resumeFailed', '载入任务记录失败：')}${(err as Error).message}`);
        });
    },
    [jumpToTask, tasks, t, pushToast],
  );

  // onSend：消息永远发进"当前任务"——进行中就直接续，没有进行中的就由记忆服务新开一个。
  // 返回值 = 是否被受理：未被受理时撤掉本地回显（界面不该显示一条其实没送出的消息），
  // 并把草稿留在输入框里让用户重试。
  const onSend = useCallback(
    async (text: string): Promise<boolean> => {
      setPortalError(null);
      // 本地先回显（受理回执只说明消息已交给 reasoner，不代表已有回复）。
      const at = new Date().toISOString();
      const echo: PortalMessage = {
        author: 'user',
        text,
        at,
        taskId: currentTask?.id ?? '',
        id: nextLocalMessageID(),
      };
      setMessages((prev) => appendMessage(prev, echo));
      try {
        await sendMessage(text);
      } catch (err) {
        setMessages((prev) => prev.filter((m) => m !== echo));
        setPortalError(`${t('ui.app.undelivered', '未送达：')}${(err as Error).message}`);
        return false;
      }
      // 新任务可能刚被创建：刷新一次任务列表（记录由 reasoner 落库，稍后才到）。
      void fetchTasks()
        .then(setTasks)
        .catch(() => undefined);
      return true;
    },
    [currentTask, t],
  );
  // 首屏：一次拉全"任务清单 + 整段会话"——面板按会话展示，任务只是里面的分节。
  useEffect(() => {
    let cancelled = false;
    void (async () => {
      try {
        const [list, history] = await Promise.all([fetchTasks(), fetchHistory(300, 0, '', true)]);
        if (cancelled) return;
        setTasks(list);
        // 与"加载期间已经到达的实时消息"合并，而不是覆盖——deliver / progress 可能先到。
        setMessages((prev) => mergeMessages(history, prev));
      } catch (err) {
        if (!cancelled) setPortalError(`${t('ui.app.historyFailed', '历史未取回：')}${(err as Error).message}`);
      }
    })();
    return () => {
      cancelled = true;
    };
    // 只跑一次：首屏把任务清单与整段会话拉下来，之后全靠 SSE 增量。
  }, []);

  // onDecide：把用户的裁决送回 approval。成功后由 SSE 的 approvals 事件把卡片删掉
  // （服务端是唯一真相：门户不自己先删，避免"界面说批了、其实 approval 已经过期"）。
  const onDecide = useCallback(async (itemId: string, decision: 'allow' | 'deny') => {
    setPortalError(null);
    try {
      await decideApproval(itemId, decision);
    } catch (err) {
      setPortalError(`${t('ui.app.decisionFailed', '裁决未送达：')}${(err as Error).message}`);
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
          aria-label={connected ? t('ui.app.connected', '已连接设置中心') : t('ui.app.disconnected', '未连接设置中心')}
          title={
            overview
              ? `${t('ui.settings.title', '设置')} ${overview.center} · ${connected ? t('ui.app.on', '已连接') : t('ui.app.off', '未连接')}`
              : t('ui.settings.title', '设置')
          }
        />
      )}

      {collapsed ? (
        <button
          type="button"
          className="sidebar sidebar-bar"
          aria-expanded={false}
          aria-label={t('ui.app.expand', '展开侧栏')}
          title={t('ui.app.expand', '展开侧栏')}
          onClick={() => {
            setCollapsed(false);
          }}
        />
      ) : (
        <aside className="sidebar">
          <div className="brand-slot" aria-hidden="true" />
          {settingsOpen ? (
            <nav className="threads" aria-label={t('ui.settings.servicesNav', '服务列表')}>
              <p className="section-label">{t('ui.settings.services', '服务')} · {services.length}</p>
              <ServiceList
                services={services}
                selected={selected}
                onSelect={(name) => {
                  selectedByUserRef.current = true;
                  setSelected(name);
                }}
                settingsCountOf={settingsCountOf}
                t={t}
              />
            </nav>
          ) : (
            <TaskList tasks={tasks} jumps={taskIdsWithRecords} current={currentTask?.id ?? ''} onJump={onJumpTask} t={t} />
          )}
          <div className="me">
            <span className="avatar" aria-hidden="true">{t('ui.me.avatar', '我')}</span>
            <span className="me-name">{t('ui.me.name', '本机用户')}</span>
            <button
              type="button"
              className="icon-btn"
              aria-expanded={true}
              aria-label={t('ui.app.collapse', '收起侧栏')}
              title={t('ui.app.collapse', '收起侧栏')}
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
            {t('ui.app.loadFailed', '加载失败：')}{loadError}
          </p>
        )}
        {settingsOpen ? (
          <SettingsView
            timeTheme={timeTheme}
            workflowExpand={workflowExpand}
            portalName={portalName}
            onClose={() => {
              setSettingsOpen(false);
            }}
            service={selectedService}
            settings={selectedSettings}
            peers={services.map((s) => s.name)}
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
              // 每次进设置页都回门户默认页（除非之后在页内亲手点了别的服务）。
              selectedByUserRef.current = false;
              if (portalName !== null) setSelected(portalName);
              setSettingsOpen(true);
            }}
            tasks={tasks}
            workflowExpand={workflowExpand.mode}
            t={t}
          />
        )}
      </main>

      <Toasts items={toasts} />
    </div>
  );
}
