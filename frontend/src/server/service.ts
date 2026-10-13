// 前端服务的 WS 端点：接受任意服务的 jump 访问。
// v1 未对外提供任何动作（面板是给人的界面，不是给服务调的接口）——
// 一律回执 unknown action，保证注册进名册的地址真实可达、行为诚实。
import type { Server as HttpServer } from 'node:http';
import { WebSocketServer, type WebSocket } from 'ws';
import type { RawData } from 'ws';
import { TypeJump, decodeEnvelope, encodeEnvelope, frameBytes, newResponse } from './protocol.ts';

export function attachJumpEndpoint(
  server: HttpServer,
  path: string,
  name: string,
  log: (m: string) => void,
): WebSocketServer {
  const wss = new WebSocketServer({ noServer: true });

  const handleConn = (ws: WebSocket): void => {
    ws.on('message', (data: RawData) => {
      let env;
      try {
        env = decodeEnvelope(frameBytes(data));
      } catch {
        log('WS 端点上收到坏帧（丢弃）');
        return;
      }
      if (env.type !== TypeJump) {
        ws.send(encodeEnvelope(newResponse(env, name, { ok: false, error: `unsupported message type: ${env.type}` })));
        return;
      }
      const jp = env.payload as { action?: string } | undefined;
      ws.send(encodeEnvelope(newResponse(env, name, { ok: false, error: `unknown action: ${jp?.action ?? ''}` })));
    });
  };

  server.on('upgrade', (req, socket, head) => {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1');
    if (url.pathname !== path) {
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => handleConn(ws));
  });

  return wss;
}
