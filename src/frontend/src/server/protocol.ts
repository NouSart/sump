// SUMP 协议 DTO（TypeScript 侧）：名册 / 设置等载荷的形状，以及消息 id 与帧字节工具。
// v1 信封（register / jump / response …）已删除——出站与入站只走 SPv2.1 原生帧（见 spv2.ts）。
import { randomBytes } from 'node:crypto';

export interface Provide {
  action: string;
  input?: string;
  output?: string;
  tool?: boolean;
}

export interface Setting {
  key: string;
  default?: string;
  readonly?: boolean;
  secret?: boolean;
  set?: boolean;
  /** 给**人**看的部分（可选）：中文名、说明、控件类型、枚举候选、数字单位。 */
  label?: string;
  hint?: string;
  kind?: string;
  options?: string[];
  unit?: string;
}

export interface RegisterPayload {
  name: string;
  addr: string;
  title?: string;
  description?: string;
  provides?: Provide[];
  settings?: Setting[];
  heartbeat_ms?: number;
}

export interface ServiceCard {
  name: string;
  addr: string;
  title?: string;
  description?: string;
  provides?: Provide[];
  heartbeat_ms?: number;
}

export interface RosterPayload {
  services: ServiceCard[];
  revision: number;
}

export interface SettingView {
  key: string;
  value: string;
  default: string;
  overridden: boolean;
  // 秘密项：值由服务自己持有，中心只回「有没有值」（secret/set），value 恒空；
  // 只读项：值来自服务启动参数，面板只展示不可改（readonly）；
  // actual：服务自报的实际值（与 value 不一致 = 服务侧自己变了）；reported 区分"没报过"与"报了空值"。
  secret?: boolean;
  set?: boolean;
  readonly?: boolean;
  actual?: string;
  reported?: boolean;
  label?: string;
  hint?: string;
  kind?: string;
  options?: string[];
}

export interface ServiceSettings {
  service: string;
  settings?: SettingView[];
}

export interface SettingsResult {
  services: ServiceSettings[];
}

// SettingResultView 是一次下发中单个设置项的结果（settings 事件）。
export interface SettingResultView {
  key: string;
  value?: string;
  ok?: boolean;
  error?: string;
}

// SettingsEventView 是 settings 事件载荷（中心 → 各服务广播；面板转 SSE）。
export interface SettingsEventView {
  cause?: string;
  service?: string;
  results?: SettingResultView[];
}

const ID_ALPHABET = '0123456789ABCDEFGHJKMNPQRSTVWXYZ';

// newID 生成 ULID 形态的消息 / 链路 id（26 字符：48 位毫秒时间 + 80 位随机）。
export function newID(): string {
  let t = Date.now();
  let time = '';
  for (let i = 0; i < 10; i++) {
    time = ID_ALPHABET.charAt(t % 32) + time;
    t = Math.floor(t / 32);
  }
  const rnd = randomBytes(16);
  let suffix = '';
  for (let i = 0; i < 16; i++) {
    suffix += ID_ALPHABET.charAt((rnd[i] ?? 0) % 32);
  }
  return time + suffix;
}

// frameBytes 把 WS 帧（Buffer / ArrayBuffer / 分片数组）统一为字节视图。
export function frameBytes(data: unknown): Uint8Array {
  if (Buffer.isBuffer(data)) return data;
  if (data instanceof Uint8Array) return data;
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (Array.isArray(data)) return Buffer.concat(data as Buffer[]);
  throw new Error('未知的帧数据类型');
}
