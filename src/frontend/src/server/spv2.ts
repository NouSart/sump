// SPv2.1 客户端：只覆盖 frontend 出站所需的最小 call/reply。
// 线上形状与 src/spv2（Go 参考实现）逐字段对齐：header 是 msgpack map，body 是 bin。
import { decode, encode } from '@msgpack/msgpack';
import WebSocket from 'ws';
import { frameBytes, newID } from './protocol.ts';

export const SPv2Version = 'spv2.1';
const maxSPv2MessageBytes = 8 << 20;

export interface Ref {
  kind: string;
  id: string;
  authority?: string;
  tenant?: string;
}

export interface Frame {
  v: string;
  id: string;
  type: string;
  work: string;
  from: Ref;
  to: Ref;
  method?: string;
  reply_to?: string;
  content_type?: string;
  status?: string;
  error?: { code?: string; message?: string; retryable?: boolean; details?: unknown };
  body?: Uint8Array;
}

// SPv2UnavailableError 表示目标没有 /spv2（拨号/握手失败），调用方可以回退 v1。
export class SPv2UnavailableError extends Error {}

// SPv2RemoteError 表示目标明确回了 status=error；API 层应映射为 400 而不是 502。
export class SPv2RemoteError extends Error {
  readonly code: string;
  constructor(code: string, message: string) {
    super(message);
    this.code = code;
  }
}

export function spv2Addr(addr: string): string {
  if (addr.endsWith('/ws')) return addr.slice(0, -3) + '/spv2';
  if (addr.endsWith('/spv2')) return addr;
  return addr.replace(/\/$/, '') + '/spv2';
}

function refOf(raw: unknown): Ref {
  const o = (raw ?? {}) as Record<string, unknown>;
  const out: Ref = {
    kind: typeof o.kind === 'string' ? o.kind : '',
    id: typeof o.id === 'string' ? o.id : '',
  };
  if (typeof o.authority === 'string' && o.authority !== '') out.authority = o.authority;
  if (typeof o.tenant === 'string' && o.tenant !== '') out.tenant = o.tenant;
  return out;
}

export function encodeFrame(f: Frame): Uint8Array {
  const out: Record<string, unknown> = {
    v: f.v,
    id: f.id,
    type: f.type,
    work: f.work,
    from: f.from,
    to: f.to,
  };
  if (f.method) out.method = f.method;
  if (f.reply_to) out.reply_to = f.reply_to;
  if (f.content_type) out.content_type = f.content_type;
  if (f.status) out.status = f.status;
  if (f.error) out.error = f.error;
  if (f.body !== undefined) out.body = f.body;
  return encode(out);
}

export function decodeFrame(bytes: Uint8Array): Frame {
  const raw = decode(bytes) as Record<string, unknown>;
  const error = raw.error as Record<string, unknown> | undefined;
  const f: Frame = {
    v: typeof raw.v === 'string' ? raw.v : '',
    id: typeof raw.id === 'string' ? raw.id : '',
    type: typeof raw.type === 'string' ? raw.type : '',
    work: typeof raw.work === 'string' ? raw.work : '',
    from: refOf(raw.from),
    to: refOf(raw.to),
  };
  if (typeof raw.method === 'string') f.method = raw.method;
  if (typeof raw.reply_to === 'string') f.reply_to = raw.reply_to;
  if (typeof raw.content_type === 'string') f.content_type = raw.content_type;
  if (typeof raw.status === 'string') f.status = raw.status;
  if (raw.body instanceof Uint8Array) f.body = raw.body;
  if (error && typeof error === 'object') {
    f.error = {
      code: typeof error.code === 'string' ? error.code : undefined,
      message: typeof error.message === 'string' ? error.message : undefined,
      retryable: typeof error.retryable === 'boolean' ? error.retryable : undefined,
      details: error.details,
    };
  }
  return f;
}

/**
 * 直连目标 /spv2 发一条 call，等 reply。
 * 只有“拨号/握手失败”抛 SPv2UnavailableError；目标明确回 error 抛 SPv2RemoteError。
 */
export function dialSPv2Once(
  addr: string,
  from: string,
  to: string,
  method: string,
  input: unknown,
  timeoutMS: number,
): Promise<unknown> {
  return new Promise<unknown>((resolve, reject) => {
    const body = input === undefined ? new Uint8Array() : encode(input);
    const call: Frame = {
      v: SPv2Version,
      id: newID(),
      type: 'call',
      work: newID(),
      from: { kind: 'service', id: from },
      to: { kind: 'service', id: to },
      method,
      content_type: 'application/msgpack',
      body,
    };
    const ws = new WebSocket(addr, { maxPayload: maxSPv2MessageBytes });
    let opened = false;
    let settled = false;
    let timer: NodeJS.Timeout | undefined;
    const finish = (fn: () => void): void => {
      if (settled) return;
      settled = true;
      if (timer !== undefined) clearTimeout(timer);
      try {
        ws.close();
      } catch {
        // 尽力
      }
      fn();
    };
    timer = setTimeout(() => {
      finish(() => reject(new Error(`SPv2 等待响应超时（${timeoutMS}ms）: ${addr}`)));
    }, timeoutMS);
    ws.on('error', (err: Error) => {
      finish(() => reject(opened ? err : new SPv2UnavailableError(err.message)));
    });
    ws.on('open', () => {
      opened = true;
      ws.send(encodeFrame(call), (err?: Error) => {
        if (err) finish(() => reject(err));
      });
    });
    ws.on('message', (data) => {
      let frame: Frame;
      try {
        frame = decodeFrame(frameBytes(data));
      } catch {
        return;
      }
      if (frame.type !== 'reply' || frame.reply_to !== call.id) return;
      if (frame.status === 'error') {
        const code = frame.error?.code || 'internal';
        const message = frame.error?.message || '远端错误';
        finish(() => reject(new SPv2RemoteError(code, message)));
        return;
      }
      let out: unknown;
      if (frame.body && frame.body.length > 0) {
        try {
          out = decode(frame.body);
        } catch (err) {
          finish(() => reject(err as Error));
          return;
        }
      }
      finish(() => resolve(out));
    });
    ws.on('close', () => {
      if (settled) return;
      finish(() => reject(opened ? new Error(`SPv2 连接在响应前关闭: ${addr}`) : new SPv2UnavailableError('连接失败')));
    });
  });
}
