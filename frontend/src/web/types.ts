// 面板视图类型：与前端服务 /api/* 的 JSON 形状对应。

export interface ProvideView {
  action: string;
  input?: string;
  output?: string;
  tool?: boolean;
}

export interface ServiceCardView {
  name: string;
  addr: string;
  /** 中文短名（服务自己报的，面板导航用）。 */
  title?: string;
  description?: string;
  provides?: ProvideView[];
  heartbeat_ms?: number;
}

export interface SettingViewData {
  key: string;
  value: string;
  default: string;
  overridden: boolean;
  // 秘密项：值由服务自己持有，中心只回「有没有值」（secret/set），Value 恒空；
  // 只读项：值来自服务启动参数，面板只展示不可改（readonly）；
  // actual：服务自报的实际值（与 value 不一致 = 服务侧自己变了）；reported 区分"没报过"与"报了空值"。
  secret?: boolean;
  set?: boolean;
  readonly?: boolean;
  actual?: string;
  reported?: boolean;
  // 人话元数据（服务声明时给出）：中文名、说明、控件类型、枚举候选。
  // 面板优先用语言包里的 setting.<service>.<key>.label/hint，其次用这里的兜底。
  label?: string;
  hint?: string;
  kind?: string;
  options?: string[];
}

// SettingResultViewData 是一次下发中单个设置项的结果（settings 事件）。
export interface SettingResultViewData {
  key: string;
  value?: string;
  ok?: boolean;
  error?: string;
}

// SettingsEventViewData 是中心广播的 settings 事件（面板经 SSE 接收）。
export interface SettingsEventViewData {
  cause?: string;
  service?: string;
  results?: SettingResultViewData[];
}

// RowStatus 是设置行的会话内瞬态状态（写入待确认 / 未送达）。
export interface RowStatus {
  state: 'pending' | 'failed';
  error?: string;
}

export interface ServiceSettingsView {
  service: string;
  settings?: SettingViewData[];
}

export interface RosterView {
  services: ServiceCardView[];
  revision: number;
}

export interface Overview {
  connected: boolean;
  center: string;
  self: { name: string; ws: string; http: string };
  roster: RosterView | null;
  settings: { services: ServiceSettingsView[] } | null;
  error?: string;
  fetchedAt: string;
}

// PortalMessage 是门户对话里的一条消息：用户发的（本地回显）或智能体经 deliver 送来的。
export interface PortalMessage {
  author: 'user' | 'agent';
  text: string;
  at: string;
}

// TimerView 是一条在计时器（timer 经 timer_state 快照推来）：门户据此画倒计时。
export interface TimerView {
  itemId: string;
  caller: string;
  ms: number;
  fireAt: string; // RFC3339：门户拿它与本地时间算剩余
  note: string;
}

// PendingApprovalView 是一条等用户裁决的命令（approval 经 approval_pending 推来）。
export interface PendingApprovalView {
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
