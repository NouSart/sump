// startFrontend 装配整个服务：HTTP 服务端（API + 静态面板）+ WS 端点（jump）+ 中心连接（注册 / 心跳 / 名册）。
import { createServer } from 'node:http';
import type { AddressInfo } from 'node:net';
import type { WebSocketServer } from 'ws';
import { createRequestHandler } from './api.ts';
import { CenterLink } from './center.ts';
import { createApprovalStore, createDeliverHub, createProgressHub, createTimerStore, parsePendingApproval, type TrustConfig } from './portal.ts';
import { attachJumpEndpoint, parseTimerItems, portalProvides, portalSettings } from './service.ts';

export const frontendDescription =
  '用户交互门户（与 qq 同类）：对外提供 deliver（智能体 → 用户，推给浏览器），自己跳 user_message 给 reasoner（用户 → 智能体）；另附设置中心可视化面板';

export interface FrontendConfig {
  name: string;
  addr: string; // host:port（host 省略时按 127.0.0.1）
  centerURL: string;
  webDir: string;
  heartbeatMS: number;
  reasoner: string;
  /** 记忆服务名：门户启动时向它要该会话的历史，用来回填对话（真相在服务，门户不自己存）。 */
  memory: string;
  /** 语言包目录（<locale>.json 的存放处）。 */
  localesDir: string;
  /** 启动语言（默认 zh-CN）。 */
  lang: string;
  /** 审批服务名（第二层裁决由门户送回它）。 */
  approval: string;
  trustedHosts: string[];
}

export interface FrontendHandle {
  link: CenterLink;
  addr: string;
  close: () => Promise<void>;
}

// splitAddr 解析 host:port，支持 [::1]:9501 这类 IPv6 字面量。
// 不接受通配地址：注册进名册的 selfURL 必须是别的服务真能回连的地址，
// 而 0.0.0.0 / :: 不是一个可回连的目标。局域网请绑定具体地址。
function splitAddr(addr: string): { host: string; port: number } {
  let host: string;
  let portText: string;
  if (addr.startsWith('[')) {
    const end = addr.indexOf(']');
    if (end < 0 || !addr.slice(end + 1).startsWith(':')) {
      throw new Error(`非法监听地址: ${addr}（IPv6 应写成 [::1]:9501）`);
    }
    host = addr.slice(1, end);
    portText = addr.slice(end + 2);
  } else {
    const idx = addr.lastIndexOf(':');
    host = (idx >= 0 ? addr.slice(0, idx) : addr) || '127.0.0.1';
    portText = idx >= 0 ? addr.slice(idx + 1) : '';
  }
  const port = Number(portText);
  if (!Number.isInteger(port) || port < 0 || port > 65535 || portText === '') {
    throw new Error(`非法监听地址: ${addr}（应为 host:port）`);
  }
  if (host === '0.0.0.0' || host === '::') {
    throw new Error(`不支持通配监听地址: ${addr}（注册的 ws 地址必须可回连，请绑定具体地址）`);
  }
  return { host, port };
}

// urlHost 给 IPv6 字面量补方括号，http:// 与 ws:// 的 authority 都需要它。
function urlHost(host: string): string {
  return host.includes(':') && !host.startsWith('[') ? `[${host}]` : host;
}

export async function startFrontend(cfg: FrontendConfig, log: (m: string) => void): Promise<FrontendHandle> {
  const { host, port } = splitAddr(cfg.addr);
  const server = createServer();
  await new Promise<void>((resolveListen, rejectListen) => {
    server.once('error', rejectListen);
    server.listen(port, host, () => resolveListen());
  });
  const actual = server.address() as AddressInfo;
  const addr = `${urlHost(actual.address)}:${actual.port}`;
  const selfURL = `ws://${addr}/ws`;

  const hub = createDeliverHub();
  const progress = createProgressHub();
  const timers = createTimerStore();
  const approvals = createApprovalStore();
  const trust: TrustConfig = { boundAddr: addr, trustedHosts: cfg.trustedHosts };
  const link = new CenterLink({
    name: cfg.name,
    title: '门户',
    centerURL: cfg.centerURL,
    selfURL,
    description: frontendDescription,
    provides: portalProvides,
    settings: portalSettings(cfg),
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
        memory: cfg.memory,
        localesDir: cfg.localesDir,
        lang: cfg.lang,
        portal: { on: hub.on },
        progress: { on: progress.on },
        timers: { current: timers.current, on: timers.on },
        approvals: { current: approvals.list, on: approvals.on, remove: approvals.remove },
        approval: cfg.approval,
        trust,
      },
      log,
    ),
  );
  const wss: WebSocketServer = attachJumpEndpoint(server, '/ws', cfg.name, log, hub.publish, timers.apply, approvals.apply, (p) => { progress.publish(p); return true; }, trust);
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

  // 待审命令同理：门户重启会丢掉内存里的待审列表，而 approval 那边还在挂起——
  // 不拉回来，用户就永远看不到这条审批（直到 TTL 到期按未通过处理）。
  let approvalSeeded = false;
  const seedApprovals = (): void => {
    if (approvalSeeded || !(link.roster?.services ?? []).some((s) => s.name === cfg.approval)) return;
    approvalSeeded = true;
    void link
      .jumpTo(cfg.approval, 'approval_list', {}, 5000)
      .then((data) => {
        const r = data as { items?: unknown };
        const items = (Array.isArray(r.items) ? r.items : [])
          .map((x) => parsePendingApproval(x, log))
          .filter((x): x is NonNullable<typeof x> => x !== null);
        approvals.replaceAll(items);
        log(`启动兜底：拉到待审项 ${String(items.length)} 条`);
      })
      .catch((err: unknown) => {
        log(`启动兜底拉取待审项失败（不影响使用）: ${String(err)}`);
      });
  };
  link.on((e) => {
    if (e.kind === 'roster') {
      seedTimers();
      seedApprovals();
    }
  });
  seedTimers();
  seedApprovals();

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
