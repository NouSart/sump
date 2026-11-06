// CenterLink 是 frontend → 设置中心 的唯一连接（服务节点一侧）：
// 注册 / 心跳 / 断线自愈（重连 + 重注册）/ 名册广播接收 / 跳转调用。
// 控制面只走 SPv2.1 原生 /spv2：register 是 call，heartbeat 是 event，roster / settings 是事件推送；
// 与 Go 侧 service/ 骨架同语义（本服务是 TS，等价实现其中这一个子集）。
import type { Provide, RegisterPayload, RosterPayload, Setting, SettingsEventView } from './protocol.ts';
import { SPv2RemoteError, SPv2Session, dialSPv2Once, spv2Addr } from './spv2.ts';
import type { SPv2NativeEvent } from './spv2.ts';

export interface CenterOptions {
  name: string;
  /** 中文短名（面板导航用）。 */
  title: string;
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
  | { kind: 'roster'; roster: RosterPayload }
  | { kind: 'settings'; event: SettingsEventView };

const callTimeoutDefault = 10_000;
const registerTimeout = 10_000;
const maxBackoff = 30_000;

// RemoteError 表示“中心明确回执的拒绝”（带原因）——
// 与传输类失败（断开 / 超时）区分，API 层据此映射 400 与 502。
export class RemoteError extends Error {}

export class CenterLink {
  private readonly opts: CenterOptions;
  private readonly listeners = new Set<(e: CenterEvent) => void>();
  private reconnectTimer: NodeJS.Timeout | null = null;
  private heartbeatTimer: NodeJS.Timeout | null = null;
  private backoff = 1_000;
  private closed = false;
  private connectedFlag = false;
  private rosterValue: RosterPayload | null = null;
  /** 最近一次成功注册：它断开说明对端是健康的，重连用基础间隔而不是继续退避。 */
  private registered = false;
  /** 当前原生 /spv2 长连接（null = 尚未连上 / 正在重连）。 */
  private native: SPv2Session | null = null;
  /** 连接代次：只认最近一次拨号的回调，旧连接的迟到事件一律忽略。 */
  private attempts = 0;

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
    this.attempts += 1;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    this.stopHeartbeat();
    const native = this.native;
    this.native = null;
    this.registered = false;
    if (native) native.close('服务已关闭');
    this.setConnected(false);
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

  // call 经中心执行一个控制面动作（等响应；未注册时立即失败）。
  // 中心明确回 status=error 时映射为 RemoteError，API 层据此回 400。
  async call(action: string, input?: unknown, timeoutMS: number = callTimeoutDefault): Promise<unknown> {
    const native = this.native;
    if (native === null || !this.connectedFlag) {
      throw new Error('未连接到设置中心');
    }
    try {
      return await native.call(action, input, timeoutMS);
    } catch (err) {
      if (err instanceof SPv2RemoteError) throw new RemoteError(err.message);
      throw err;
    }
  }

  // jumpTo 跳转到名册里的另一个服务并等响应——门户的"用户 → 智能体"这一跳走它。
  // 帧的 from 带发起方自己：一跳的归属就是发起的服务（对方最终经 deliver 直达 from）——
  // 少了它，请求照样送达，但回答会找不到回来的路（reasoner 只认发起方）。
  async jumpTo(to: string, action: string, input?: unknown, timeoutMS: number = callTimeoutDefault): Promise<unknown> {
    const card = this.rosterValue?.services?.find((c) => c.name === to);
    if (!card || !card.addr) throw new Error(`未知目标服务: ${to}（不在名册里）`);
    try {
      return await dialSPv2Once(spv2Addr(card.addr), this.opts.name, card.name, action, input, timeoutMS);
    } catch (err) {
      if (err instanceof SPv2RemoteError) throw new RemoteError(err.message);
      throw err;
    }
  }

  private connect(): void {
    if (this.closed) return;
    const attempt = ++this.attempts;
    const addr = spv2Addr(this.opts.centerURL);
    void SPv2Session.dial(addr, this.opts.name, 'settings-center', {
      onEvent: (ev) => {
        if (attempt === this.attempts) this.onNativeEvent(ev);
      },
      onClose: () => {
        if (attempt === this.attempts) this.onNativeClose();
      },
      onError: (err) => {
        this.opts.log(`设置中心（SPv2）连接出错: ${err.message}`);
      },
    })
      .then((session) => {
        if (this.closed || attempt !== this.attempts) {
          session.close('服务已关闭');
          return;
        }
        this.native = session;
        void this.registerNative(session);
      })
      .catch((err: unknown) => {
        if (this.closed || attempt !== this.attempts) return;
        this.scheduleReconnect(false, `SPv2 连接设置中心失败: ${(err as Error).message}`);
      });
  }

  // 注册名片：原生 register call 的 body。
  private registerPayload(): RegisterPayload {
    return {
      name: this.opts.name,
      title: this.opts.title,
      addr: this.opts.selfURL,
      description: this.opts.description,
      provides: this.opts.provides,
      settings: this.opts.settings,
      heartbeat_ms: this.opts.heartbeatMS,
    };
  }

  private async registerNative(session: SPv2Session): Promise<void> {
    try {
      const data = await session.call('register', this.registerPayload(), registerTimeout);
      if (this.closed || this.native !== session) {
        session.close('服务已关闭');
        return;
      }
      const roster = (data ?? { services: [], revision: 0 }) as RosterPayload;
      this.setRoster(roster);
      this.registered = true;
      this.backoff = 1_000;
      this.setConnected(true);
      this.startHeartbeat(session);
      this.opts.log(`已接入设置中心（SPv2 原生，名册 ${roster.services?.length ?? 0} 个服务, rev ${roster.revision ?? 0}）`);
    } catch (err) {
      if (this.closed || this.native !== session) return;
      this.native = null;
      this.registered = false;
      this.stopHeartbeat();
      this.setConnected(false);
      session.close('SPv2 注册失败');
      this.scheduleReconnect(false, `SPv2 注册设置中心失败: ${(err as Error).message}`);
    }
  }

  private onNativeClose(): void {
    if (this.closed) return;
    const wasRegistered = this.registered;
    this.native = null;
    this.registered = false;
    this.stopHeartbeat();
    this.setConnected(false);
    this.scheduleReconnect(wasRegistered, '与设置中心（SPv2）连接断开');
  }

  // scheduleReconnect 统一退避语义：注册成功过的连接断开只等基础间隔（对端健康，中心重启要快回来）；
  // 连不上 / 注册不上才逐步翻倍，避免每秒刷屏。
  private scheduleReconnect(wasRegistered: boolean, reason: string): void {
    if (this.closed) return;
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer);
      this.reconnectTimer = null;
    }
    const delay = wasRegistered ? 1_000 : this.backoff;
    this.backoff = wasRegistered ? 1_000 : Math.min(this.backoff * 2, maxBackoff);
    this.opts.log(`${reason}，${delay}ms 后重连`);
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null;
      this.connect();
    }, delay);
  }

  private onNativeEvent(ev: SPv2NativeEvent): void {
    if (ev.method === 'roster') {
      const roster = ev.body as RosterPayload | undefined;
      if (roster && typeof roster === 'object') this.setRoster(roster);
      return;
    }
    if (ev.method === 'settings') {
      const payload = ev.body as SettingsEventView | undefined;
      if (payload && typeof payload === 'object') this.emit({ kind: 'settings', event: payload });
      return;
    }
    this.opts.log(`忽略未知 SPv2 事件: ${ev.method}`);
  }

  private startHeartbeat(session: SPv2Session): void {
    this.stopHeartbeat();
    this.heartbeatTimer = setInterval(() => {
      if (this.closed || this.native !== session) return;
      if (!session.sendEvent('heartbeat', { status: 'ok' })) {
        this.opts.log('SPv2 心跳发送失败');
      }
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
}
