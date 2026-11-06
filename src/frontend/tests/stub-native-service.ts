// StubNativeService：测试用假目标服务（真 WS、真 SPv2.1 帧）。
// 记录收到的 call（method / input / from / to），按 replyFor 生成 reply；
// from.id 就是原生链路里的回程归属（v1 的 boss 等价物）。
import { decode, encode } from '@msgpack/msgpack';
import { WebSocketServer, type RawData, type WebSocket } from 'ws';
import { decodeFrame, encodeFrame, type Frame } from '../src/server/spv2.ts';
import { frameBytes, newID } from '../src/server/protocol.ts';

export interface NativeCall {
  method: string;
  input: unknown;
  /** 发起方服务名：原生帧的 from.id，回答要沿它回去。 */
  from: string;
  to: string;
}

export interface NativeReply {
  ok: boolean;
  data?: unknown;
  error?: string;
}

export class StubNativeService {
  readonly calls: NativeCall[] = [];
  /** 按 method / input 生成回执；默认成功且无数据。 */
  replyFor: (method: string, input: unknown) => NativeReply = () => ({ ok: true });

  private wss: WebSocketServer | null = null;
  private url = '';

  async start(): Promise<string> {
    const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
    this.wss = wss;
    wss.on('connection', (ws: WebSocket) => {
      ws.on('message', (data: RawData) => this.onMessage(ws, frameBytes(data)));
    });
    await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
    const address = wss.address() as { port: number };
    this.url = `ws://127.0.0.1:${address.port}/spv2`;
    return this.url;
  }

  async stop(): Promise<void> {
    const wss = this.wss;
    this.wss = null;
    if (wss === null) return;
    for (const ws of wss.clients) ws.terminate();
    await new Promise<void>((resolve) => wss.close(() => resolve()));
  }

  private onMessage(ws: WebSocket, bytes: Uint8Array): void {
    let frame: Frame;
    try {
      frame = decodeFrame(bytes);
    } catch {
      return;
    }
    if (frame.type !== 'call') return;
    const input = frame.body !== undefined && frame.body.length > 0 ? (decode(frame.body) as unknown) : undefined;
    const method = frame.method ?? '';
    this.calls.push({ method, input, from: frame.from.id, to: frame.to.id });
    const reply = this.replyFor(method, input);
    const out: Frame = {
      v: 'spv2.1',
      id: newID(),
      type: 'reply',
      work: frame.work,
      from: { kind: 'service', id: frame.to.id === '' ? 'stub' : frame.to.id },
      to: frame.from,
      reply_to: frame.id,
      status: reply.ok ? 'ok' : 'error',
      content_type: 'application/msgpack',
    };
    if (reply.ok) {
      if (reply.data !== undefined) out.body = encode(reply.data);
    } else {
      out.error = { code: 'invalid_argument', message: reply.error ?? 'stub 拒绝', retryable: false };
    }
    ws.send(encodeFrame(out));
  }
}
