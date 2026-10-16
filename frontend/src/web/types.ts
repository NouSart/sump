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
  description?: string;
  provides?: ProvideView[];
  heartbeat_ms?: number;
}

export interface SettingViewData {
  key: string;
  value: string;
  default: string;
  overridden: boolean;
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
