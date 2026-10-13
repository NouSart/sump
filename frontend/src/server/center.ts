// CenterLink 是 frontend → 设置中心 的唯一连接（服务节点一侧）：
// 注册 / 心跳 / 断线自愈（重连 + 重注册）/ 名册广播接收 / 跳转调用。
// 与 Go 侧 service/ 骨架同语义（本服务是 TS，等价实现其中这一个子集）。
import WebSocket, { type RawData } from 'ws';
import type { Envelope, Provide, RegisterPayload, ResponsePayloadView, RosterPayload, Setting } from './protocol.ts';
import {
  TypeHeartbeat,
  TypeJump,
  TypeRegister,
  TypeResponse,
  TypeRoster,
  decodeEnvelope,
  encodeEnvelope,
  frameBytes,
  newEnvelope,
} from './protocol.ts';

export interface CenterOptions {
  name: string;
  centerURL: string;
  selfURL: string;
  description: string;
  provides: Provide[];
  settings: Setting[];
  heartbeatMS: number;
  log: (msg: string) => void;
}

export type CenterEvent =
  | { kind: 'status'; connected: boolean }
  | { kind: 'roster'; roster: RosterPayload };

interface Pending {
  resolve: (v: unknown) => void;
  reject: (e: Error) => void;
  timer: NodeJS.Timeout;
}

const callTimeoutDefault = 10_000;
const registerTimeout = 10_000;
const maxBackoff = 30_000;

// RemoteError 表示“中心明确回执的拒绝”（ok=false，带原因）——
// 与传输类失败（断开 / 超时）区分，API 层据此映射 400 与 502。
export class RemoteError extends Error {}

export class CenterLink {
  private readonly opts: CenterOptions;
  private ws: WebSocket | null = null;
  private readonly pending = new Map<string, Pending>();
  private readonly listeners = new Set<(e: CenterEvent) => void>();
  private reconnectTimer: NodeJS.Timeout | null = null;
  private heartbeatTimer: NodeJS.Timeout | null = null;
  private backoff = 1_000;
  private closed = false;
  private connectedFlag = false;
  private rosterValue: RosterPayload | null = null;

  constructor(opts: CenterOptions) {
    this.opts = opts;
  }

  start(): void {
    this.closed = false;
    this.connect();
  }

  // stop 主动关闭：不再重连；未决调用全部失败。测试与进程退出使用。
  stop(): void {
    this.closed = true;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.stopHeartbeat();
    const ws = this.ws;
    this.ws = null;
    this.setConnected(false);
    if (ws) {
      try {
        ws.terminate();
      } catch {
        // 尽力
      }
    }
    this.rejectAll(new Error('服务已关闭'));
  }

  get connected(): boolean {
    return this.connectedFlag;
  }

  get roster(): RosterPayload | null {
    return this.rosterValue;
  }

  // on 订阅状态 / 名册事件；返回退订函数。
  on(fn: (e: CenterEvent) => void): () => void {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  // call 经中心跳转执行一个动作（等响应；未连接时立即失败）。
  async call(action: string, input?: unknown, timeoutMS: number = callTimeoutDefault): Promise<unknown> {
    const ws = this.ws;
    if (!ws || !this.connectedFlag || ws.readyState !== WebSocket.OPEN) {
      throw new Error('未连接到设置中心');
    }
    const body: Record<string, unknown> = { action };
    if (input !== undefined) body.input = input;
    const env = newEnvelope(TypeJump, this.opts.name, 'settings-center', '', body);
    return this.callSocket(ws, env, timeoutMS);
  }

  private connect(): void {
    if (this.closed) return;
    const ws = new WebSocket(this.opts.centerURL);
    this.ws = ws;
    ws.on('open', () => {
      void this.register(ws);
    });
    ws.on('message', (data: RawData) => {
      try {
        this.onMessage(frameBytes(data));
      } catch (err) {
        this.opts.log(`消息处理失败: ${(err as Error).message}`);
      }
    });
    ws.on('error', (err: Error) => {
      this.opts.log(`连接设置中心出错: ${err.message}`);
    });
    ws.on('close', () => {
      this.onClose(ws);
    });
  }

  private async register(ws: WebSocket): Promise<void> {
    const payload: RegisterPayload = {
      name: this.opts.name,
      addr: this.opts.selfURL,
      description: this.opts.description,
      provides: this.opts.provides,
      settings: this.opts.settings,
      heartbeat_ms: this.opts.heartbeatMS,
    };
    const env = newEnvelope(TypeRegister, this.opts.name, 'settings-center', '', payload);
    try {
      const data = await this.callSocket(ws, env, registerTimeout);
      if (this.closed || this.ws !== ws) return;
      const roster = (data ?? { services: [], revision: 0 }) as RosterPayload;
      this.setRoster(roster);
      this.backoff = 1_000;
      this.setConnected(true);
      this.startHeartbeat(ws);
      this.opts.log(`已接入设置中心（名册 ${roster.services?.length ?? 0} 个服务, rev ${roster.revision ?? 0}）`);
    } catch (err) {
      this.opts.log(`注册设置中心失败: ${(err as Error).message}`);
      this.backoff = Math.min(this.backoff * 2, maxBackoff);
      try {
        ws.terminate();
      } catch {
        // 尽力
      }
    }
  }

  private callSocket(ws: WebSocket, env: Envelope, timeoutMS: number): Promise<unknown> {
    return new Promise<unknown>((resolve, reject) => {
      const timer = setTimeout(() => {
        this.pending.delete(env.id);
        reject(new Error(`等待响应超时（${timeoutMS}ms）`));
      }, timeoutMS);
      this.pending.set(env.id, { resolve, reject, timer });
      ws.send(encodeEnvelope(env), (err) => {
        if (!err) return;
        clearTimeout(timer);
        this.pending.delete(env.id);
        reject(err);
      });
    });
  }

  private onMessage(bytes: Uint8Array): void {
    let env: Envelope;
    try {
      env = decodeEnvelope(bytes);
    } catch (err) {
      this.opts.log(`坏帧: ${(err as Error).message}`);
      return;
    }
    if (env.type === TypeResponse) {
      const p = this.pending.get(env.id);
      if (!p) return;
      this.pending.delete(env.id);
      clearTimeout(p.timer);
      const rp = env.payload as ResponsePayloadView | undefined;
      if (!rp || typeof rp !== 'object') {
        p.reject(new Error('响应缺少 payload'));
        return;
      }
      if (rp.ok !== true) {
        p.reject(new RemoteError(rp.error || '远端错误'));
        return;
      }
      p.resolve(rp.data);
      return;
    }
    if (env.type === TypeRoster) {
      const roster = env.payload as RosterPayload | undefined;
      if (roster && typeof roster === 'object') this.setRoster(roster);
    }
  }

  private onClose(ws: WebSocket): void {
    if (this.ws !== ws) return;
    this.ws = null;
    this.stopHeartbeat();
    this.setConnected(false);
    this.rejectAll(new Error('与设置中心连接断开'));
    if (this.closed) return;
    const delay = this.backoff;
    this.opts.log(`与设置中心连接断开，${delay}ms 后重连`);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  private startHeartbeat(ws: WebSocket): void {
    this.stopHeartbeat();
    this.heartbeatTimer = setInterval(() => {
      if (this.ws !== ws || ws.readyState !== WebSocket.OPEN) return;
      const env = newEnvelope(TypeHeartbeat, this.opts.name, 'settings-center', '', { status: 'ok' });
      ws.send(encodeEnvelope(env), (err) => {
        if (!err) return;
        try {
          ws.terminate();
        } catch {
          // 尽力
        }
      });
    }, this.opts.heartbeatMS);
  }

  private stopHeartbeat(): void {
    if (this.heartbeatTimer) {
      clearInterval(this.heartbeatTimer);
      this.heartbeatTimer = null;
    }
  }

  private setRoster(roster: RosterPayload): void {
    this.rosterValue = roster;
    this.emit({ kind: 'roster', roster });
  }

  private setConnected(connected: boolean): void {
    if (this.connectedFlag === connected) return;
    this.connectedFlag = connected;
    this.emit({ kind: 'status', connected });
  }

  private emit(e: CenterEvent): void {
    for (const fn of [...this.listeners]) fn(e);
  }

  private rejectAll(err: Error): void {
    for (const [, p] of this.pending) {
      clearTimeout(p.timer);
      p.reject(err);
    }
    this.pending.clear();
  }
}
