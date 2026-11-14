// SPv2.1 客户端：只覆盖 frontend 出站所需的最小 call/reply。
// 线上形状与 src/spv2（Go 参考实现）逐字段对齐：header 是 msgpack map，body 是 bin。
import { decode, encode } from '@msgpack/msgpack';
import WebSocket, { WebSocketServer, type RawData } from 'ws';
import { frameBytes, newID } from './protocol.ts';
import {
  PORTAL_CONVERSATION_ID,
  checkRequestTrust,
  parsePendingApproval,
  parseProgress,
  parseTerminalUpdate,
  type DeliverMessage,
  type PendingApprovalItem,
  type ProgressUpdate,
  type TerminalUpdate,
  type TimerState,
  type TrustConfig,
} from './portal.ts';
import { parseTimerItems } from './service.ts';

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

// SPv2UnavailableError 表示目标没有 /spv2（拨号/握手失败）——
// 与“目标明确回 error”（SPv2RemoteError）区分，API 层据此映射 502。
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
    const callId = newID();
    const helloId = newID();
    const call: Frame = {
      v: SPv2Version,
      id: callId,
      type: 'call',
      work: newID(),
      from: { kind: 'service', id: from },
      to: { kind: 'service', id: to },
      method,
      content_type: 'application/msgpack',
      body,
    };
    // spec §5.1：连接后第一帧必须是 hello；握手成功后才发真正的 call。
    const hello: Frame = {
      v: SPv2Version,
      id: helloId,
      type: 'call',
      work: newID(),
      from: { kind: 'service', id: from },
      to: { kind: 'service', id: to },
      method: 'hello',
      content_type: 'application/msgpack',
      body: encode({ protocol: SPv2Version, capabilities: ['msgpack', 'json', 'stream'] }),
    };
    const ws = new WebSocket(addr, { maxPayload: maxSPv2MessageBytes });
    let opened = false;
    let settled = false;
    let helloDone = false;
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
      ws.send(encodeFrame(hello), (err?: Error) => {
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
      if (!helloDone) {
        if (frame.type !== 'reply' || frame.reply_to !== helloId) return;
        if (frame.status === 'error') {
          const code = frame.error?.code || 'internal';
          finish(() => reject(new SPv2RemoteError(code, frame.error?.message || '握手失败')));
          return;
        }
        helloDone = true;
        ws.send(encodeFrame(call), (err?: Error) => {
          if (err) finish(() => reject(err));
        });
        return;
      }
      if (frame.type !== 'reply' || frame.reply_to !== callId) return;
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
// SPv2NativeEvent 是原生长连接收到的事件（method + 已解码 body）。
export interface SPv2NativeEvent {
  method: string;
  body?: unknown;
}

export interface SPv2SessionHandlers {
  onEvent?: (ev: SPv2NativeEvent) => void;
  /** 连接被对端关闭 / 网络断开时调用；主动 close() 不触发。 */
  onClose?: () => void;
  onError?: (err: Error) => void;
}

interface SPv2PendingCall {
  resolve: (v: unknown) => void;
  reject: (e: Error) => void;
  timer: NodeJS.Timeout;
}

// 握手超时：中心 /spv2 黑洞（SYN 有去无回）时，不能让 CenterLink 永远卡在“连接中”。
const spv2HandshakeTimeoutMS = 5_000;
// call 的默认等待时限（CenterLink 显式传入时以调用方为准）。
const spv2CallTimeoutDefault = 10_000;

/**
 * SPv2Session 是一条可复用的 SPv2.1 长连接：call 等 reply，event 走回调。
 * 与 dialSPv2Once 的区别是连接不随单次调用关闭——设置中心控制面要持续收 roster / settings 事件。
 */
export class SPv2Session {
  private readonly ws: WebSocket;
  private readonly from: Ref;
  private readonly to: Ref;
  private readonly handlers: SPv2SessionHandlers;
  private readonly pending = new Map<string, SPv2PendingCall>();
  private closedFlag = false;
  private opened = false;

  private constructor(ws: WebSocket, from: string, to: string, handlers: SPv2SessionHandlers) {
    this.ws = ws;
    this.from = { kind: 'service', id: from, authority: 'none' };
    this.to = { kind: 'service', id: to };
    this.handlers = handlers;
    ws.on('message', (data) => {
      this.onMessage(frameBytes(data));
    });
    ws.on('error', (err: Error) => {
      // 握手前由 dial 统一 reject，这里不重复上报。
      if (this.closedFlag || !this.opened) return;
      this.handlers.onError?.(err);
    });
  }

  /** 建立连接；拨号/握手失败抛 SPv2UnavailableError。 */
  static dial(addr: string, from: string, to: string, handlers: SPv2SessionHandlers = {}): Promise<SPv2Session> {
    return new Promise<SPv2Session>((resolve, reject) => {
      let ws: WebSocket;
      try {
        ws = new WebSocket(addr, {
          maxPayload: maxSPv2MessageBytes,
          handshakeTimeout: spv2HandshakeTimeoutMS,
        });
      } catch (err) {
        reject(new SPv2UnavailableError((err as Error).message));
        return;
      }
      const session = new SPv2Session(ws, from, to, handlers);
      let settled = false;
      ws.on('open', () => {
        if (settled) return;
        session.opened = true;
        // spec §5.1：握手成功后才把 session 交给调用方，保证第一条业务帧之前已经有 hello。
        session
          .call('hello', { protocol: SPv2Version, capabilities: ['msgpack', 'json', 'stream'] })
          .then(() => {
            if (settled) return;
            settled = true;
            resolve(session);
          })
          .catch((err: Error) => {
            if (settled) return;
            settled = true;
            session.closedFlag = true;
            try {
              ws.close();
            } catch {
              // 尽力
            }
            reject(new SPv2UnavailableError(err.message));
          });
      });
      ws.on('error', (err: Error) => {
        if (settled) return;
        settled = true;
        session.closedFlag = true;
        reject(new SPv2UnavailableError(err.message));
      });
      ws.on('close', () => {
        if (!settled) {
          settled = true;
          session.closedFlag = true;
          reject(new SPv2UnavailableError('连接失败'));
          return;
        }
        session.handleSocketClose();
      });
    });
  }

  get closed(): boolean {
    return this.closedFlag;
  }

  /** 发 call 并等 reply；目标明确回 error 抛 SPv2RemoteError。 */
  call(method: string, input: unknown, timeoutMS = spv2CallTimeoutDefault): Promise<unknown> {
    if (this.closedFlag || !this.opened || this.ws.readyState !== WebSocket.OPEN) {
      return Promise.reject(new Error('SPv2 连接不可用'));
    }
    const id = newID();
    const frame: Frame = {
      v: SPv2Version,
      id,
      type: 'call',
      work: newID(),
      from: this.from,
      to: this.to,
      method,
      content_type: 'application/msgpack',
      body: input === undefined ? new Uint8Array() : encode(input),
    };
    return new Promise<unknown>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(id);
        reject(new Error(`SPv2 等待响应超时（${timeoutMS}ms）`));
      }, timeoutMS);
      this.pending.set(id, { resolve, reject, timer });
      try {
        this.ws.send(encodeFrame(frame), (err?: Error) => {
          if (!err) return;
          const p = this.pending.get(id);
          if (p === undefined) return;
          clearTimeout(p.timer);
          this.pending.delete(id);
          reject(err);
        });
      } catch (err) {
        const p = this.pending.get(id);
        if (p !== undefined) {
          clearTimeout(p.timer);
          this.pending.delete(id);
        }
        reject(err as Error);
      }
    });
  }

  /** 发 event（不等 reply）；连接不可用时返回 false。 */
  sendEvent(method: string, input: unknown): boolean {
    if (this.closedFlag || !this.opened || this.ws.readyState !== WebSocket.OPEN) return false;
    const frame: Frame = {
      v: SPv2Version,
      id: newID(),
      type: 'event',
      work: newID(),
      from: this.from,
      to: this.to,
      method,
      content_type: 'application/msgpack',
      body: input === undefined ? undefined : encode(input),
    };
    try {
      this.ws.send(encodeFrame(frame));
      return true;
    } catch (err) {
      this.handlers.onError?.(err as Error);
      return false;
    }
  }

  /** 主动关闭：不再回调 onClose；未决 call 全部失败。 */
  close(reason = 'SPv2 连接已关闭'): void {
    if (this.closedFlag) return;
    this.closedFlag = true;
    this.rejectPending(new Error(reason));
    try {
      this.ws.close();
    } catch {
      // 尽力
    }
  }

  private onMessage(bytes: Uint8Array): void {
    if (this.closedFlag) return;
    let frame: Frame;
    try {
      frame = decodeFrame(bytes);
    } catch (err) {
      this.handlers.onError?.(err as Error);
      return;
    }
    if (frame.type === 'reply') {
      const replyTo = frame.reply_to ?? '';
      const p = this.pending.get(replyTo);
      if (p === undefined) return;
      this.pending.delete(replyTo);
      clearTimeout(p.timer);
      if (frame.status === 'error') {
        p.reject(new SPv2RemoteError(frame.error?.code || 'internal', frame.error?.message || '远端错误'));
        return;
      }
      if (frame.body === undefined || frame.body.length === 0) {
        p.resolve(undefined);
        return;
      }
      try {
        p.resolve(decode(frame.body));
      } catch (err) {
        p.reject(err as Error);
      }
      return;
    }
    if (frame.type === 'event') {
      let body: unknown;
      if (frame.body !== undefined && frame.body.length > 0) {
        try {
          body = decode(frame.body);
        } catch (err) {
          this.handlers.onError?.(err as Error);
          return;
        }
      }
      this.handlers.onEvent?.({ method: frame.method ?? '', body });
    }
  }

  private handleSocketClose(): void {
    if (this.closedFlag) return;
    this.closedFlag = true;
    this.rejectPending(new Error('SPv2 连接已断开'));
    this.handlers.onClose?.();
  }

  private rejectPending(err: Error): void {
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
      p.reject(err);
    }
    this.pending.clear();
  }
}

export interface SPv2PortalHandlers {
  onDeliver: (m: DeliverMessage) => void;
  // authorizeDeliver 校验 deliver 归属（spec §6.5）：返回 null 放行，否则返回拒绝原因。
  // 生产必须接；未配置时 deliver 一律 fail-closed，不允许默认信任 from。
  authorizeDeliver?: (frame: Frame) => Promise<string | null>;
  onTimers: (s: TimerState) => boolean;
  onApproval: (item: PendingApprovalItem) => void;
  onProgress: (p: ProgressUpdate) => boolean;
  onTerminal: (u: TerminalUpdate) => boolean;
}

// createSPv2WSS 生成 /spv2 的端口；upgrade 路由由 service.attachSPv2Endpoint 统一处理，
// 避免同一 HTTP server 上两个 upgrade 监听互相抢/互相 destroy。
export function createSPv2WSS(name: string, log: (m: string) => void, handlers: SPv2PortalHandlers): WebSocketServer {
  const wss = new WebSocketServer({ noServer: true, maxPayload: 1 << 20 });
  wss.on('connection', (ws: WebSocket) => {
    // spec §5.1：连接后第一帧必须是 hello；未握手前只回错误，不执行业务动作。
    let helloDone = false;
    ws.on('error', (err: Error) => {
      log(`SPv2 WS 连接出错（即将关闭）: ${err.message}`);
    });
    ws.on('message', (data: RawData) => {
      let frame: Frame;
      try {
        frame = decodeFrame(frameBytes(data));
      } catch {
        log('SPv2 端点上收到坏帧（丢弃）');
        return;
      }
      if (frame.type !== 'call') return;
      const input = frame.body && frame.body.length > 0 ? (decode(frame.body) as Record<string, unknown>) : {};
      const method = frame.method ?? '';
      const reply = (status: 'ok' | 'error', body?: unknown, error?: { code: string; message: string; retryable: boolean }): void => {
        const out: Frame = {
          v: SPv2Version,
          id: newID(),
          type: 'reply',
          work: frame.work,
          from: { kind: 'service', id: name },
          to: frame.from,
          reply_to: frame.id,
          status,
        };
        if (body !== undefined) out.body = encode(body);
        if (error !== undefined) out.error = error;
        ws.send(encodeFrame(out));
      };

      if (method === 'hello') {
        helloDone = true;
        reply('ok', {
          card: {
            protocol: SPv2Version,
            name,
            actor: { kind: 'service', id: name },
            version: 'spv2-ts/0.1',
            capabilities: ['msgpack', 'json'],
            methods: [],
            limits: { max_body: 1 << 20, max_stream: 32 },
            observability: { health: false, metrics: false, format: '' },
          },
        });
        return;
      }
      if (!helloDone) {
        reply('error', undefined, { code: 'invalid_argument', message: '连接首帧必须是 hello', retryable: false });
        return;
      }

      if (method === 'deliver') {
        const text = typeof input.text === 'string' ? input.text : '';
        if (text === '') {
          reply('error', undefined, { code: 'invalid_argument', message: 'deliver 需要非空 text', retryable: false });
          return;
        }
        const conversationId =
          typeof input.conversation_id === 'string' && input.conversation_id !== ''
            ? input.conversation_id
            : PORTAL_CONVERSATION_ID;
        const taskId = typeof input.task_id === 'string' ? input.task_id : '';
        // spec §6.5：deliver 只有 work.agent+runtime 或 work.owner+owner 能发。
        // 校验是异步的（要问 memory 的 work 行），失败一律 permission_denied，不静默放行。
        const authorize = handlers.authorizeDeliver;
        const check = async (): Promise<void> => {
          if (authorize === undefined) {
            reply('error', undefined, { code: 'permission_denied', message: 'deliver 未配置归属校验（fail-closed）', retryable: false });
            return;
          }
          let denied: string | null;
          try {
            denied = await authorize(frame);
          } catch (err) {
            denied = 'deliver 归属校验失败（fail-closed）: ' + String(err);
          }
          if (denied !== null) {
            reply('error', undefined, { code: 'permission_denied', message: denied, retryable: false });
            return;
          }
          handlers.onDeliver({ text, conversationId, taskId, at: new Date().toISOString() });
          reply('ok', { delivered: true });
        };
        void check();
        return;
      }
      if (method === 'timer_state') {
        const accepted = handlers.onTimers({
          boss: typeof input.boss === 'string' ? input.boss : '',
          seq: typeof input.seq === 'number' ? input.seq : 0,
          items: parseTimerItems(input.timers, log),
        });
        reply('ok', { accepted });
        return;
      }
      if (method === 'approval_pending') {
        const item = parsePendingApproval(input, log);
        if (item !== null) handlers.onApproval(item);
        reply('ok', { accepted: item !== null });
        return;
      }
      if (method === 'progress') {
        const p = parseProgress(input, log);
        const accepted = p !== null ? handlers.onProgress(p) : false;
        reply('ok', { accepted });
        return;
      }
      if (method === 'report_result') {
        const u = parseTerminalUpdate(input, log);
        const accepted = u !== null ? handlers.onTerminal(u) : false;
        reply('ok', { accepted });
        return;
      }
      reply('error', undefined, { code: 'not_found', message: `unknown method: ${method}`, retryable: false });
    });
  });
  return wss;
}


// checkDeliverAuthority 是 spec §6.5 的 deliver 归属判定（纯函数，便于单测）：
// 只放行 work.agent+authority=runtime，或 work.owner+authority=owner。
export function checkDeliverAuthority(
  work: { agent?: { kind?: string; id?: string }; owner?: { kind?: string; id?: string } } | undefined,
  from: { kind: string; id: string; authority?: string },
): string | null {
  if (work === undefined) return 'work 不存在，拒绝 deliver';
  if (from.kind === 'agent' && from.authority === 'runtime' && from.id === work.agent?.id) return null;
  if (from.kind === 'human' && from.authority === 'owner' && from.id === work.owner?.id) return null;
  return (
    'deliver 来源无权限（from=' + from.kind + ':' + from.id + '/' + (from.authority ?? '') + '）'
  );
}

// checkSPv2Trust 与门户 HTTP 层共用同一道信任栅栏。
export function checkSPv2Trust(reqHost: string | undefined, reqOrigin: string | undefined, trust: TrustConfig): string | null {
  return checkRequestTrust(reqHost, reqOrigin, trust);
}
