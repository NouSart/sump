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
