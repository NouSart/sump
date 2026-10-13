// StubCenter：测试用假设置中心（真 WS、真 msgpack 字节），
// 覆盖 register / heartbeat / jump / roster 广播——与 Go 侧设置中心行为等价的最小集合。
// 注意：payload 按 Go 的 RawMessage 语义内联为“嵌套值”（map），不是 bin 字节块。
import { decode, encode } from '@msgpack/msgpack';
import { WebSocketServer, type RawData, type WebSocket } from 'ws';

interface WireEnvelope {
  v: number;
  id: string;
  trace?: string;
  type: string;
  from: string;
  to: string;
  payload?: unknown;
}

function toU8(data: RawData): Uint8Array {
  if (Buffer.isBuffer(data)) return data;
  if (Array.isArray(data)) return Buffer.concat(data);
  return new Uint8Array(data);
}

export class StubCenter {
  registrations: Record<string, unknown>[] = [];
  heartbeats = 0;
  jumps: { action: string; input: unknown; from: string }[] = [];
  roster: { services: unknown[]; revision: number } = { services: [], revision: 1 };
  settingsReply: { services: unknown[] } = { services: [] };
  setError = '';

  private wss: WebSocketServer | null = null;
  private readonly conns = new Set<WebSocket>();
  private seq = 0;
  private url = '';

  async start(): Promise<string> {
    const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
    this.wss = wss;
    wss.on('connection', (ws) => {
      this.conns.add(ws);
      ws.on('close', () => this.conns.delete(ws));
      ws.on('message', (data: RawData) => this.onMessage(ws, toU8(data)));
    });
    await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
    const address = wss.address() as { port: number };
    this.url = `ws://127.0.0.1:${address.port}/ws`;
    return this.url;
  }

  async stop(): Promise<void> {
    this.closeConnections();
    if (this.wss) {
      const wss = this.wss;
      this.wss = null;
      await new Promise<void>((resolve) => wss.close(() => resolve()));
    }
  }

  closeConnections(): void {
    for (const ws of this.conns) ws.terminate();
    this.conns.clear();
  }

  clientCount(): number {
    return this.conns.size;
  }

  broadcastRoster(roster?: { services: unknown[]; revision: number }): void {
    if (roster) this.roster = roster;
    const bytes = encode({
      v: 1,
      id: `roster-${++this.seq}`,
      type: 'roster',
      from: 'settings-center',
      to: '*',
      payload: this.roster,
    });
    for (const ws of this.conns) ws.send(bytes);
  }

  private onMessage(ws: WebSocket, bytes: Uint8Array): void {
    const env = decode(bytes) as unknown as WireEnvelope;
    switch (env.type) {
      case 'register': {
        const reg = (env.payload as Record<string, unknown> | undefined) ?? {};
        this.registrations.push(reg);
        this.reply(ws, env, { ok: true, data: this.roster });
        return;
      }
      case 'heartbeat': {
        this.heartbeats++;
        return;
      }
      case 'jump': {
        const jp = (env.payload as { action?: string; input?: unknown } | undefined) ?? {};
        const action = jp.action ?? '';
        const input = jp.input;
        this.jumps.push({ action, input, from: env.from });
        if (action === 'list_settings') {
          this.reply(ws, env, { ok: true, data: this.settingsReply });
          return;
        }
        if (action === 'set_setting') {
          if (this.setError) {
            this.reply(ws, env, { ok: false, error: this.setError });
            return;
          }
          const p = input as { service: string; key: string; value: string };
          this.reply(ws, env, {
            ok: true,
            data: {
              services: [
                { service: p.service, settings: [{ key: p.key, value: p.value, default: '', overridden: true }] },
              ],
            },
          });
          return;
        }
        if (action === 'reset_setting') {
          const p = input as { service: string; key: string };
          this.reply(ws, env, {
            ok: true,
            data: {
              services: [
                { service: p.service, settings: [{ key: p.key, value: 'd', default: 'd', overridden: false }] },
              ],
            },
          });
          return;
        }
        this.reply(ws, env, { ok: false, error: `unknown action: ${action}` });
        return;
      }
      default:
        return;
    }
  }

  private reply(ws: WebSocket, req: WireEnvelope, body: { ok: boolean; data?: unknown; error?: string }): void {
    const out: Record<string, unknown> = {
      v: 1,
      id: req.id,
      type: 'response',
      from: 'settings-center',
      to: req.from,
      payload: body,
    };
    if (req.trace) out.trace = req.trace;
    ws.send(encode(out));
  }
}
