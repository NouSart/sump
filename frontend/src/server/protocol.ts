// SUMP v2 协议（TypeScript 侧）：信封 + MessagePack 编解码。
// 与 Go 侧 protocol/ 逐字段对齐——msgpack 的键名即线路键名（snake_case），
// 兼容性由 tests/protocol.test.ts 中的 Go 产出夹具钉死（双向字节级验证）。
import { decode, encode } from '@msgpack/msgpack';
import { randomBytes } from 'node:crypto';

export const ProtocolVersion = 1;

// 消息类型（信封 type 字段的取值，与 Go 侧一致）。
export const TypeRegister = 'register';
export const TypeResponse = 'response';
export const TypeRoster = 'roster';
export const TypeJump = 'jump';
export const TypeHeartbeat = 'heartbeat';
export const TypeSettings = 'settings';

export interface Provide {
  action: string;
  input?: string;
  output?: string;
  tool?: boolean;
}

export interface Setting {
  key: string;
  default?: string;
}

export interface RegisterPayload {
  name: string;
  addr: string;
  description?: string;
  provides?: Provide[];
  settings?: Setting[];
  heartbeat_ms?: number;
}

export interface ServiceCard {
  name: string;
  addr: string;
  description?: string;
  provides?: Provide[];
  heartbeat_ms?: number;
}

export interface RosterPayload {
  services: ServiceCard[];
  revision: number;
}

export interface ResponsePayload {
  ok: boolean;
  data?: unknown;
  error?: string;
}

// ResponsePayloadView 是解码侧视图（字段可能缺失）。
export interface ResponsePayloadView {
  ok?: boolean;
  data?: unknown;
  error?: string;
}

export interface JumpPayload {
  action: string;
  input?: unknown;
}

export interface HeartbeatPayload {
  status?: string;
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

export interface Envelope {
  v: number;
  id: string;
  trace?: string;
  boss?: string;
  type: string;
  from: string;
  to: string;
  payload?: unknown;
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

// 关于 payload 的线路形状（重要）：
// Go 侧 Envelope.Payload 是 msgpack.RawMessage——编码时直接内联为“嵌套值”（map / 数组 / …），
// 不是 bin 字节块。因此 TS 侧 payload 就是普通 JS 值：编解码都只有一层，不做二次包装。

// encodeEnvelope 把信封编码为线路字节；空字段与 Go 的 omitempty 对齐（不出现键）。
export function encodeEnvelope(env: Envelope): Uint8Array {
  const out: Record<string, unknown> = {
    v: env.v,
    id: env.id,
    type: env.type,
    from: env.from,
    to: env.to,
  };
  if (env.trace) out.trace = env.trace;
  if (env.boss) out.boss = env.boss;
  if (env.payload !== undefined) out.payload = env.payload;
  return encode(out);
}

export function decodeEnvelope(bytes: Uint8Array): Envelope {
  const raw = decode(bytes) as Record<string, unknown>;
  return {
    v: typeof raw.v === 'number' ? raw.v : 0,
    id: typeof raw.id === 'string' ? raw.id : '',
    trace: typeof raw.trace === 'string' ? raw.trace : undefined,
    boss: typeof raw.boss === 'string' ? raw.boss : undefined,
    type: typeof raw.type === 'string' ? raw.type : '',
    from: typeof raw.from === 'string' ? raw.from : '',
    to: typeof raw.to === 'string' ? raw.to : '',
    payload: raw.payload ?? undefined,
  };
}

// newEnvelope 构造一条消息：自动填协议版本与 id（trace 为空时自动生成）。
export function newEnvelope(type: string, from: string, to: string, trace: string, payload?: unknown): Envelope {
  return {
    v: ProtocolVersion,
    id: newID(),
    trace: trace || newID(),
    type,
    from,
    to,
    payload,
  };
}

// newResponse 构造对 req 的响应：沿用请求的 id（配对）与 trace（同链）。
export function newResponse(req: Envelope, from: string, payload: ResponsePayload): Envelope {
  const env = newEnvelope(TypeResponse, from, req.from, req.trace ?? '', payload);
  env.id = req.id;
  return env;
}

// frameBytes 把 WS 帧（Buffer / ArrayBuffer / 分片数组）统一为字节视图。
export function frameBytes(data: unknown): Uint8Array {
  if (Buffer.isBuffer(data)) return data;
  if (data instanceof Uint8Array) return data;
  if (data instanceof ArrayBuffer) return new Uint8Array(data);
  if (Array.isArray(data)) return Buffer.concat(data as Buffer[]);
  throw new Error('未知的帧数据类型');
}
