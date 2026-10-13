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
