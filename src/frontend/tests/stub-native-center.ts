// StubNativeCenter：测试用假设置中心（原生 SPv2 /spv2 控制面）。
// 覆盖 register call / heartbeat event / roster & settings event / list_settings call；
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

  private wss: WebSocketServer | null = null;
  private readonly conns = new Map<WebSocket, { kind: string; id: string }>();
  private url = '';

  async start(): Promise<string> {
    const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
    this.wss = wss;
    wss.on('connection', (ws) => {
      ws.on('close', () => this.conns.delete(ws));
      ws.on('message', (data: RawData) => this.onMessage(ws, toU8(data)));
    });
    await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
    const address = wss.address() as { port: number };
    this.url = `ws://127.0.0.1:${address.port}/ws`;
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
      this.conns.set(ws, frame.from);
      this.reply(ws, frame, this.roster);
      return;
    }
    if (method === 'list_settings') {
      this.reply(ws, frame, this.settingsReply);
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
