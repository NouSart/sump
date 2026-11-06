// StubNativeCenter：测试用假设置中心（原生 SPv2 /spv2 控制面）。
// 覆盖 register call / heartbeat event / roster & settings event / 设置控制面 call；
// 帧形状与 src/settings-center/server/native.go 的实现对齐。
import { decode, encode } from '@msgpack/msgpack';
import { WebSocketServer, type RawData, type WebSocket } from 'ws';
import { decodeFrame, encodeFrame, type Frame } from '../src/server/spv2.ts';
import { frameBytes, newID } from '../src/server/protocol.ts';

function toU8(data: RawData): Uint8Array {
  return frameBytes(data);
}

export class StubNativeCenter {
  registrations: Record<string, unknown>[] = [];
  heartbeats = 0;
  heartbeatBodies: unknown[] = [];
  calls: { method: string; input: unknown; from: string }[] = [];
  roster: { services: unknown[]; revision: number } = { services: [], revision: 1 };
  settingsReply: { services: unknown[] } = { services: [] };
  /** 非空时 set_setting 回这个错误：验证 API 把中心拒绝映射成 400。 */
  setError = '';
  /** 非空时 register 一律回错误：验证“注册不上会长退避重试”而不是死循环刷屏。 */
  registerError = '';
  /** true 时拒绝所有握手：验证“拨号失败会退避重试”。 */
  down = false;
  /** 握手尝试次数（含被拒的）。 */
  attempts = 0;

  private wss: WebSocketServer | null = null;
  private readonly conns = new Map<WebSocket, { kind: string; id: string }>();
  private url = '';

  async start(): Promise<string> {
    const wss = new WebSocketServer({
      port: 0,
      host: '127.0.0.1',
      verifyClient: () => {
        this.attempts++;
        return !this.down;
      },
    });
    this.wss = wss;
    wss.on('connection', (ws) => {
      ws.on('close', () => this.conns.delete(ws));
      ws.on('message', (data: RawData) => this.onMessage(ws, toU8(data)));
    });
    await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
    const address = wss.address() as { port: number };
    this.url = `ws://127.0.0.1:${address.port}/spv2`;
    return this.url;
  }

  async stop(): Promise<void> {
    for (const ws of this.conns.keys()) ws.terminate();
    this.conns.clear();
    if (this.wss) {
      const wss = this.wss;
      this.wss = null;
      await new Promise<void>((resolve) => wss.close(() => resolve()));
    }
  }

  closeConnections(): void {
    for (const ws of [...this.conns.keys()]) ws.terminate();
    this.conns.clear();
  }

  broadcastRoster(roster?: { services: unknown[]; revision: number }): void {
    if (roster) this.roster = roster;
    this.broadcastEvent('roster', this.roster);
  }

  broadcastSettings(payload: unknown): void {
    this.broadcastEvent('settings', payload);
  }

  private broadcastEvent(method: string, body: unknown): void {
    for (const [ws, to] of this.conns) {
      const frame: Frame = {
        v: 'spv2.1',
        id: newID(),
        type: 'event',
        work: newID(),
        from: { kind: 'service', id: 'settings-center' },
        to,
        method,
        content_type: 'application/msgpack',
        body: encode(body),
      };
      ws.send(encodeFrame(frame));
    }
  }

  private onMessage(ws: WebSocket, bytes: Uint8Array): void {
    let frame: Frame;
    try {
      frame = decodeFrame(bytes);
    } catch {
      return;
    }
    if (frame.type === 'event') {
      if (frame.method === 'heartbeat') {
        this.heartbeats++;
        this.heartbeatBodies.push(frame.body && frame.body.length > 0 ? decode(frame.body) : undefined);
      }
      return;
    }
    if (frame.type !== 'call') return;
    const input = frame.body && frame.body.length > 0 ? (decode(frame.body) as unknown) : undefined;
    const method = frame.method ?? '';
    this.calls.push({ method, input, from: frame.from.id });
    if (method === 'register') {
      const reg = (input as Record<string, unknown> | undefined) ?? {};
      this.registrations.push(reg);
      if (this.registerError !== '') {
        this.replyError(ws, frame, this.registerError);
        return;
      }
      this.conns.set(ws, frame.from);
      this.reply(ws, frame, this.roster);
      return;
    }
    if (method === 'list_settings') {
      this.reply(ws, frame, this.settingsReply);
      return;
    }
    if (method === 'set_setting') {
      if (this.setError !== '') {
        this.replyError(ws, frame, this.setError);
        return;
      }
      const p = input as { service: string; key: string; value: string };
      this.reply(ws, frame, {
        services: [{ service: p.service, settings: [{ key: p.key, value: p.value, default: '', overridden: true }] }],
      });
      return;
    }
    if (method === 'reset_setting') {
      const p = input as { service: string; key: string };
      this.reply(ws, frame, {
        services: [{ service: p.service, settings: [{ key: p.key, value: 'd', default: 'd', overridden: false }] }],
      });
      return;
    }
    this.replyError(ws, frame, `unknown method: ${method}`);
  }

  private reply(ws: WebSocket, req: Frame, body: unknown): void {
    const out: Frame = {
      v: 'spv2.1',
      id: newID(),
      type: 'reply',
      work: req.work,
      from: { kind: 'service', id: 'settings-center' },
      to: req.from,
      reply_to: req.id,
      status: 'ok',
      content_type: 'application/msgpack',
      body: encode(body),
    };
    ws.send(encodeFrame(out));
  }

  private replyError(ws: WebSocket, req: Frame, message: string): void {
    const out: Frame = {
      v: 'spv2.1',
      id: newID(),
      type: 'reply',
      work: req.work,
      from: { kind: 'service', id: 'settings-center' },
      to: req.from,
      reply_to: req.id,
      status: 'error',
      content_type: 'application/msgpack',
      error: { code: 'invalid_argument', message, retryable: false },
    };
    ws.send(encodeFrame(out));
  }
}
