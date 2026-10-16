// startFrontend 装配整个服务：HTTP 服务端（API + 静态面板）+ WS 端点（jump）+ 中心连接（注册 / 心跳 / 名册）。
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import type { WebSocketServer } from 'ws';
import { createRequestHandler } from './api.ts';
import { CenterLink } from './center.ts';
import { createDeliverHub, createTimerStore, type TrustConfig } from './portal.ts';
import { attachJumpEndpoint, parseTimerItems, portalProvides } from './service.ts';

export const frontendDescription =
  '用户交互门户（与 qq 同类）：对外提供 deliver（智能体 → 用户，推给浏览器），自己跳 user_message 给 reasoner（用户 → 智能体）；另附设置中心可视化面板';

export interface FrontendConfig {
  name: string;
  addr: string; // host:port（host 省略时按 127.0.0.1）
  centerURL: string;
  webDir: string;
  heartbeatMS: number;
  reasoner: string;
  trustedHosts: string[];
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

  const hub = createDeliverHub();
  const timers = createTimerStore();
  const trust: TrustConfig = { boundAddr: addr, trustedHosts: cfg.trustedHosts };
  const link = new CenterLink({
    name: cfg.name,
    centerURL: cfg.centerURL,
    selfURL,
    description: frontendDescription,
    provides: portalProvides,
    settings: [],
    heartbeatMS: cfg.heartbeatMS,
    log,
  });

  server.on(
    'request',
    createRequestHandler(
      link,
      {
        name: cfg.name,
        httpAddr: addr,
        selfURL,
        centerURL: cfg.centerURL,
        webDir: cfg.webDir,
        reasoner: cfg.reasoner,
        portal: { on: hub.on },
        timers: { current: timers.current, on: timers.on },
        trust,
      },
      log,
    ),
  );
  const wss: WebSocketServer = attachJumpEndpoint(server, '/ws', cfg.name, log, hub.publish, timers.apply, trust);
  link.start();

  // 冷启动兜一次：门户重启后 timer 不会主动重推在计时器，这里拉一次 list 补上。
  // 失败不影响任何功能（下一次变动 timer 会推快照过来），所以只记日志、不重试。
  let seeded = false;
  const seedTimers = (): void => {
    if (seeded || !(link.roster?.services ?? []).some((s) => s.name === 'timer')) return;
    seeded = true;
    void link
      .jumpTo('timer', 'list', {}, 5000)
      .then((data) => {
        const r = data as { seq?: unknown; timers?: unknown };
        const accepted = timers.apply({
          boss: '',
          seq: typeof r.seq === 'number' ? r.seq : 0,
          items: parseTimerItems(r.timers, log),
        });
        log(`启动兜底：拉到在计时器（采纳=${String(accepted)}）`);
      })
      .catch((err: unknown) => {
        log(`启动兜底拉取在计时器失败（不影响使用）: ${String(err)}`);
      });
  };
  link.on((e) => {
    if (e.kind === 'roster') seedTimers();
  });
  seedTimers();

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
