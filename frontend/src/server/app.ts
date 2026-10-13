// startFrontend 装配整个服务：HTTP 服务端（API + 静态面板）+ WS 端点（jump）+ 中心连接（注册 / 心跳 / 名册）。
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import type { WebSocketServer } from 'ws';
import { createRequestHandler } from './api.ts';
import { CenterLink } from './center.ts';
import { attachJumpEndpoint } from './service.ts';

export const frontendDescription =
  '设置中心可视化面板：注册为服务节点（心跳 / 名册广播），向浏览器提供 HTTP JSON + SSE；界面不持业务状态';

export interface FrontendConfig {
  name: string;
  addr: string; // host:port（host 省略时按 127.0.0.1）
  centerURL: string;
  webDir: string;
  heartbeatMS: number;
}

export interface FrontendHandle {
  link: CenterLink;
  addr: string;
  close: () => Promise<void>;
}

function splitAddr(addr: string): { host: string; port: number } {
  const idx = addr.lastIndexOf(':');
  const host = (idx >= 0 ? addr.slice(0, idx) : addr) || '127.0.0.1';
  const port = idx >= 0 ? Number(addr.slice(idx + 1)) : Number.NaN;
  if (!Number.isInteger(port) || port < 0 || port > 65535) {
    throw new Error(`非法监听地址: ${addr}（应为 host:port）`);
  }
  return { host, port };
}

export async function startFrontend(cfg: FrontendConfig, log: (m: string) => void): Promise<FrontendHandle> {
  const { host, port } = splitAddr(cfg.addr);
  const server = createServer();
  await new Promise<void>((resolveListen, rejectListen) => {
    server.once('error', rejectListen);
    server.listen(port, host, () => resolveListen());
  });
  const actual = server.address() as AddressInfo;
  const addr = `${actual.address}:${actual.port}`;
  const selfURL = `ws://${addr}/ws`;

  const link = new CenterLink({
    name: cfg.name,
    centerURL: cfg.centerURL,
    selfURL,
    description: frontendDescription,
    provides: [],
    settings: [],
    heartbeatMS: cfg.heartbeatMS,
    log,
  });

  server.on(
    'request',
    createRequestHandler(link, { name: cfg.name, httpAddr: addr, selfURL, centerURL: cfg.centerURL, webDir: cfg.webDir }, log),
  );
  const wss: WebSocketServer = attachJumpEndpoint(server, '/ws', cfg.name, log);
  link.start();

  return {
    link,
    addr,
    close: async () => {
      link.stop();
      for (const ws of wss.clients) ws.terminate();
      await new Promise<void>((resolveClose) => wss.close(() => resolveClose()));
      await new Promise<void>((resolveClose) => {
        server.close(() => resolveClose());
        server.closeAllConnections();
      });
    },
  };
}
