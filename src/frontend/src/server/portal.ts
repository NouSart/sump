// 门户（单用户）：会话身份、投递分发、浏览器信任栅栏。
//
// 定位：frontend 与 qq 是**同类型的用户交互门户**——对外提供 deliver（智能体 → 用户），
// 自己 jump user_message 给 reasoner（用户 → 智能体）。设置面板则是它的另一页。
//
// 单用户：会话 id 形状与 qq 的 qq:private:<uid> 一致。
// 默认会话保留老 id（历史记录都挂在它下面）；新会话在它后面接短后缀。
export const DEFAULT_CONVERSATION_ID = 'web:private:owner';
/** 兼容旧名字：默认会话。 */
export const PORTAL_CONVERSATION_ID = DEFAULT_CONVERSATION_ID;

// newPortalConversationId 生成一个新会话 id（时间 + 随机，可读且不撞）。
export function newPortalConversationId(now = Date.now(), rand = Math.random): string {
  const noise = Math.floor(rand() * 0x100000000).toString(36);
  return `${DEFAULT_CONVERSATION_ID}-${now.toString(36)}${noise}`;
}

// isPortalConversationId 判断一个 id 是不是**门户自己的**会话。
//
// 这不是格式洁癖，是隐私边界：门户能通过 HTTP 指定 conversation_id，
// 若不设限，浏览器就能拿它去读 qq:private:* 那类别的入口的会话。
export function isPortalConversationId(id: string): boolean {
  return id === DEFAULT_CONVERSATION_ID || id.startsWith(`${DEFAULT_CONVERSATION_ID}-`);
}

export interface DeliverMessage {
  text: string;
  conversationId: string;
  /** 这条答复属于哪个任务（门户按任务分段展示时用）。 */
  taskId: string;
  at: string;
}

// DeliverHub 把 WS 端点上收到的投递分发给所有 SSE 订阅者（浏览器可能开着多个标签页）。
export interface DeliverHub {
  publish(m: DeliverMessage): void;
  on(fn: (m: DeliverMessage) => void): () => void;
}

export function createDeliverHub(): DeliverHub {
  const listeners = new Set<(m: DeliverMessage) => void>();
  return {
    publish: (m) => {
      for (const fn of [...listeners]) fn(m);
    },
    on: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
}

// ---------- 任务实况（reasoner → 门户 → 浏览器）----------
//
// 实况是**过程**不是结果：思考、终端命令、终端输出。门户只负责转手给浏览器，
// 不做任何判断——真相在记忆里（history），这里只是让界面不必等到最后才看见。

export interface ProgressUpdate {
  conversationId: string;
  /** 这条实况属于哪个任务（门户按任务分段展示时用）。 */
  taskId: string;
  seq: number;
  kind: string; // reasoning / tool_call / tool_result
  content: string;
  meta: string; // JSON 字符串（工具名、call_id、是否截断…）
  at: string;
}

export interface ProgressHub {
  publish(p: ProgressUpdate): void;
  on(fn: (p: ProgressUpdate) => void): () => void;
}

export function createProgressHub(): ProgressHub {
  const listeners = new Set<(p: ProgressUpdate) => void>();
  return {
    publish: (p) => {
      for (const fn of [...listeners]) fn(p);
    },
    on: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
}

// parseProgress 校验一条实况：形状不对就丢（单条丢弃不影响整轮对话）。

export function parseProgress(raw: unknown, log: (m: string) => void): ProgressUpdate | null {
  const o = raw as Record<string, unknown> | null;
  if (o === null || typeof o !== 'object') return null;
  const kind = typeof o.kind === 'string' ? o.kind : '';
  const content = typeof o.content === 'string' ? o.content : '';
  if (kind === '' || content === '') {
    log('progress 形状不符（缺 kind 或 content，丢弃）');
    return null;
  }
  return {
    conversationId: typeof o.conversation_id === 'string' ? o.conversation_id : '',
    taskId: typeof o.task_id === 'string' ? o.task_id : '',
    seq: typeof o.seq === 'number' && Number.isFinite(o.seq) ? o.seq : 0,
    kind,
    content,
    meta: typeof o.meta === 'string' ? o.meta : '',
    at: typeof o.at === 'string' ? o.at : new Date().toISOString(),
  };
}

// ---------- 终端（shell 补报 → 门户 → 浏览器）----------
//
// 终端与对话并行：用户在面板里直接敲命令，门户把它交给 shell（boss=自己），
// shell 执行完经 report_result 把结果送回来，门户原样转给浏览器。
// 审批不绕过——shell 照样找 approval，待审项的 item_id 就是这里的 itemId。

export interface TerminalUpdate {
  /** shell 的任务 id（也是审批项 id：终端据此显示"等待审批"）。 */
  itemId: string;
  /** 这条命令属于哪个门户会话（发起时记下；shell 的补报里没有这个字段）。 */
  conversationId?: string;
  /** queued / approved / rejected / done / failed。 */
  phase: string;
  command: string;
  /** 补报的文字说明（审批阶段用得多：为什么拒、批准了）。 */
  text: string;
  exitCode: number;
  stdout: string;
  stderr: string;
  error: string;
  truncated: boolean;
  timedOut: boolean;
  durationMs: number;
  /** shell 说这条是终态（approved 是过程态）。 */
  final: boolean;
  at: string;
}

export interface TerminalHub {
  publish(u: TerminalUpdate): void;
  on(fn: (u: TerminalUpdate) => void): () => void;
  /** 最近的几条（新 SSE 连接补发用：刷新页面还能看见刚跑完的输出）。 */
  recent: () => TerminalUpdate[];
}

export function createTerminalHub(keep = 20): TerminalHub {
  const listeners = new Set<(u: TerminalUpdate) => void>();
  const buf: TerminalUpdate[] = [];
  return {
    publish: (u) => {
      buf.push(u);
      if (buf.length > keep) buf.splice(0, buf.length - keep);
      for (const fn of [...listeners]) fn(u);
    },
    on: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
    recent: () => [...buf],
  };
}

// parseTerminalUpdate 校验一条 shell 补报：形状不对就丢（单条丢弃不影响别的命令）。
// data 是 ShellEventData（msgpack RawMessage 在 JS 侧是嵌套对象）。
export function parseTerminalUpdate(raw: unknown, log: (m: string) => void): TerminalUpdate | null {
  const o = raw as { item_id?: unknown; phase?: unknown; text?: unknown; final?: unknown; data?: unknown } | null;
  const itemId = typeof o?.item_id === 'string' ? o.item_id : '';
  const phase = typeof o?.phase === 'string' ? o.phase : '';
  if (itemId === '' || phase === '') {
    log('report_result 形状不符（缺 item_id 或 phase，丢弃）');
    return null;
  }
  const data = (o?.data ?? {}) as Record<string, unknown>;
  const str = (v: unknown): string => (typeof v === 'string' ? v : '');
  const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  return {
    itemId,
    phase,
    command: str(data.command),
    text: str(o?.text),
    exitCode: num(data.exit_code),
    stdout: str(data.stdout),
    stderr: str(data.stderr),
    error: str(data.error),
    truncated: data.truncated === true,
    timedOut: data.timed_out === true,
    durationMs: num(data.duration_ms),
    final: o?.final === true,
    at: new Date().toISOString(),
  };
}

// ---------- 计时器快照（timer → 门户 → 浏览器）----------
//
// timer 在每次受理 / 到点 / 撤销之后推一条**快照**过来（不是事件），门户据此展示倒计时。
// 存快照而不是攒事件：门户重启、浏览器新开标签，拿到的都是当前真相，不会因为错过一条
// 事件就漂。快照带单调递增的 seq——timer 用的是骨架的 Fire（各自成 goroutine，**到达顺序
// 无保证**），门户只采纳比已见序号大的那条。
//
// boss 是这批计时的归属（信封 boss 原样带来）：门户据此认领该给谁看；空 boss 归默认会话。

export interface TimerItem {
  itemId: string;
  caller: string;
  ms: number;
  fireAt: string; // RFC3339（门户用它与本地时间算剩余）
  note: string;
}

export interface TimerState {
  boss: string;
  seq: number;
  items: TimerItem[];
}

export interface TimerStore {
  /** apply 收下一条快照；序号不比已见的旧才采纳（乱序到达的自然丢弃）。 */
  apply(state: TimerState): boolean;
  /** current 返回当前所有在计时器（按到点时刻升序）。 */
  current(): TimerItem[];
  /** on 订阅变化（只推变化；初值请先取一次 current）。 */
  on(fn: (items: TimerItem[]) => void): () => void;
}

export function createTimerStore(): TimerStore {
  const byBoss = new Map<string, TimerState>();
  const listeners = new Set<(items: TimerItem[]) => void>();

  const flatten = (): TimerItem[] =>
    [...byBoss.values()]
      .flatMap((s) => s.items)
      .sort((a, b) => a.fireAt.localeCompare(b.fireAt));

  const emit = (): void => {
    const items = flatten();
    for (const fn of [...listeners]) fn(items);
  };

  return {
    apply: (state) => {
      const prev = byBoss.get(state.boss);
      if (prev !== undefined && state.seq <= prev.seq) return false;
      // 空快照也留着（记住序号，继续拒旧），只是不再贡献计时。
      byBoss.set(state.boss, state);
      emit();
      return true;
    },
    current: flatten,
    on: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
}

// ---------- 待审命令（approval → 门户 → 浏览器）----------
//
// 第二层审批：approval 判不出来（白名单没覆盖）的命令会挂起，并把待审项推给门户。
// 门户只做两件事：把它推给浏览器、把用户的裁决送回 approval。
// **不在这里记历史**：approval 那边一次只认一条在挂的项，裁决完即从列表里删掉。

export interface PendingApprovalItem {
  itemId: string;
  command: string;
  workDir: string;
  requestedBy: string;
  reasonCode: string;
  summary: string;
  unknowns: string[];
  ruleIds: string[];
  createdMs: number;
  expiresMs: number;
}

/** parsePendingApproval 校验 approval 推来的待审项：外部输入不可信，形状不对整条丢弃。 */
export function parsePendingApproval(raw: unknown, log: (m: string) => void): PendingApprovalItem | null {
  const o = raw as Record<string, unknown> | null;
  if (o === null || typeof o !== 'object') return null;
  if (typeof o.item_id !== 'string' || o.item_id === '' || typeof o.command !== 'string') {
    log('approval_pending 形状不符（缺 item_id 或 command，丢弃）');
    return null;
  }
  const strs = (v: unknown): string[] => (Array.isArray(v) ? v.filter((x): x is string => typeof x === 'string') : []);
  const num = (v: unknown): number => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
  return {
    itemId: o.item_id,
    command: o.command,
    workDir: typeof o.work_dir === 'string' ? o.work_dir : '',
    requestedBy: typeof o.requested_by === 'string' ? o.requested_by : '',
    reasonCode: typeof o.reason_code === 'string' ? o.reason_code : '',
    summary: typeof o.summary === 'string' ? o.summary : '',
    unknowns: strs(o.unknowns),
    ruleIds: strs(o.rule_ids),
    createdMs: num(o.created_ms),
    expiresMs: num(o.expires_ms),
  };
}

export interface ApprovalStore {
  /** apply 新增或替换一条待审项（同一 item_id 幂等）。 */
  apply(item: PendingApprovalItem): void;
  /** remove 删掉一条（已裁决 / 已过期）。 */
  remove(itemId: string): void;
  /** replaceAll 用全量列表替换（门户重启后从 approval 拉回）。 */
  replaceAll(items: PendingApprovalItem[]): void;
  /** list 返回当前待审项（按创建时间升序）。 */
  list(): PendingApprovalItem[];
  /** on 订阅变化（只推变化；初值请先取一次 list）。 */
  on(fn: (items: PendingApprovalItem[]) => void): () => void;
}

export function createApprovalStore(): ApprovalStore {
  const byId = new Map<string, PendingApprovalItem>();
  const listeners = new Set<(items: PendingApprovalItem[]) => void>();
  const snapshot = (): PendingApprovalItem[] => [...byId.values()].sort((a, b) => a.createdMs - b.createdMs);
  const emit = (): void => {
    const items = snapshot();
    for (const fn of [...listeners]) fn(items);
  };
  return {
    apply: (item) => {
      byId.set(item.itemId, item);
      emit();
    },
    remove: (itemId) => {
      if (byId.delete(itemId)) emit();
    },
    replaceAll: (items) => {
      byId.clear();
      for (const it of items) byId.set(it.itemId, it);
      emit();
    },
    list: snapshot,
    on: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
}

// ---------- 浏览器信任栅栏 ----------
//
// 为什么需要它：门户一旦能驱动智能体（而智能体手里有 shell），它就是一个"能执行命令的入口"。
// 只绑回环并不够——恶意页面可以把域名解析到 127.0.0.1（DNS rebinding）：浏览器看它是**同源**，
// 于是不发 CORS 预检，但 Host 头写着攻击者的域名。Host 是重绑伪造不了的那个头，所以栅栏钉它。
//
// 与 DSH 的 api-request-trust.ts 同一思路。注意：**它只证明"请求来自受信地址"，不建立身份**。

export interface TrustConfig {
  /** 实际绑定的 host:port，用于放行"用绑定地址直接访问"这一情形。 */
  boundAddr: string;
  /** 额外放行的 Host（`-trusted-hosts`，逗号分隔）。留给反代 / 局域网域名。 */
  trustedHosts: readonly string[];
}

/** isLoopbackHostname 判断主机名是否指向本机回环（与 DSH 的 loopback-hostname.ts 同判据）。 */
export function isLoopbackHostname(hostname: string): boolean {
  if (hostname === 'localhost' || hostname === '::1' || hostname === '[::1]') return true;
  const parts = hostname.split('.');
  return parts.length === 4
    && parts[0] === '127'
    && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
}

/** splitHostPort 拆 `host[:port]`；IPv6 字面量保留方括号。 */
function splitHostPort(value: string): { hostname: string; port: string } {
  if (value.startsWith('[')) {
    const end = value.indexOf(']');
    if (end < 0) return { hostname: value, port: '' };
    const rest = value.slice(end + 1);
    return { hostname: value.slice(0, end + 1), port: rest.startsWith(':') ? rest.slice(1) : '' };
  }
  const idx = value.lastIndexOf(':');
  // 无冒号，或冒号后不是端口（如裸 IPv6），整体当主机名。
  if (idx < 0 || !/^\d+$/.test(value.slice(idx + 1))) return { hostname: value, port: '' };
  return { hostname: value.slice(0, idx), port: value.slice(idx + 1) };
}

function isTrustedHost(host: string, bound: { hostname: string; port: string }, trusted: readonly string[]): boolean {
  const { hostname, port } = splitHostPort(host);
  if (trusted.includes(host) || trusted.includes(hostname)) return true;
  if (host === bound.hostname + (bound.port === '' ? '' : `:${bound.port}`)) return true;
  // 端口必须对上：不同端口是另一个服务，没有理由放行。
  if (port !== '' && port !== bound.port) return false;
  if (isLoopbackHostname(hostname)) return true;
  // 显式绑到非回环地址时，放行用该地址访问自身。
  return !isLoopbackHostname(bound.hostname) && hostname === bound.hostname;
}

/**
 * checkRequestTrust 校验一次 HTTP 请求是否来自受信地址。
 * @param hostHeader - 请求的 Host 头（浏览器必然带；重绑攻击伪造不了）。
 * @param originHeader - 请求的 Origin 头（跨站请求会带；同源读请求不带，故缺省不算错）。
 * @param cfg - 绑定地址与额外放行清单。
 * @returns 拒绝原因；通过则返回 null。
 */
export function checkRequestTrust(
  hostHeader: string | undefined,
  originHeader: string | undefined,
  cfg: TrustConfig,
): string | null {
  const bound = splitHostPort(cfg.boundAddr);
  if (hostHeader === undefined || hostHeader.trim() === '') return '缺少 Host 头';
  const host = hostHeader.trim().toLowerCase();
  if (!isTrustedHost(host, { hostname: bound.hostname.toLowerCase(), port: bound.port }, cfg.trustedHosts.map((h) => h.trim().toLowerCase()))) {
    return `Host 不在受信范围: ${host}（如确需从其他地址访问，用 -trusted-hosts 显式放行）`;
  }
  // 'null' 是沙箱 iframe / file:// 页面序列化出来的 Origin：它不是"没有 Origin"，
  // 而是"来源不可核实的页面"，与跨站同等拒绝。
  if (originHeader !== undefined) {
    const origin = originHeader.trim();
    if (origin.toLowerCase() === 'null') {
      return `Origin 不受信: ${originHeader}（沙箱 / file:// 页面一律拒绝）`;
    }
    let originHost: string;
    try {
      originHost = new URL(origin).host.toLowerCase();
    } catch {
      return `非法 Origin: ${originHeader}`;
    }
    if (!isTrustedHost(originHost, { hostname: bound.hostname.toLowerCase(), port: bound.port }, cfg.trustedHosts.map((h) => h.trim().toLowerCase()))) {
      return `Origin 不在受信范围: ${originHeader}`;
    }
  }
  return null;
}
