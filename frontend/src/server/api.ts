// 面板的 HTTP 层：浏览器 JSON API + SSE 事件流 + 静态资源。
// 只做“翻译”：把 HTTP 请求翻成中心跳转，把回执翻回 JSON——不缓存业务数据、不落盘。
import { createReadStream } from 'node:fs';
import { readdir, readFile } from 'node:fs/promises';
import { stat } from 'node:fs/promises';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { extname, join, resolve, sep } from 'node:path';
import type { CenterEvent, CenterLink } from './center.ts';
import { RemoteError } from './center.ts';
import { PORTAL_CONVERSATION_ID, checkRequestTrust, type DeliverMessage, type PendingApprovalItem, type TimerItem, type TrustConfig } from './portal.ts';
import type { RosterPayload, ServiceSettings, SettingsResult } from './protocol.ts';

export interface ApiConfig {
  name: string;
  httpAddr: string;
  selfURL: string;
  centerURL: string;
  webDir: string;
  /** 门户的对话对象：用户消息跳给它（与 qq 的 -reasoner 同名同义）。 */
  reasoner: string;
  /** 记忆服务名：历史回填直连它取（中心只提供名册地址，不转发）。 */
  memory: string;
  /** 语言包目录（扫描其中的 <locale>.json）。 */
  localesDir: string;
  /** 启动语言：服务端自用（浏览器可另选，见 /api/i18n）。 */
  lang: string;
  /** 投递订阅（deliver → SSE）。 */
  portal: { on: (fn: (m: DeliverMessage) => void) => () => void };
  /** 计时快照订阅（timer_state → SSE）：新连接先补当前值，再推变化。 */
  timers: { current: () => TimerItem[]; on: (fn: (items: TimerItem[]) => void) => () => void };
  /** 待审命令：approval 推来的挂起项（approval_pending），以及用户裁决后的移除。 */
  approvals: {
    current: () => PendingApprovalItem[];
    on: (fn: (items: PendingApprovalItem[]) => void) => () => void;
    remove: (itemId: string) => void;
  };
  /** 审批服务名（裁决经 jumpTo 送到它那里；默认 approval）。 */
  approval: string;
  /** 浏览器信任栅栏的放行范围。 */
  trust: TrustConfig;
}

class HttpError extends Error {
  readonly status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
  }
}

const contentTypes: Record<string, string> = {
  '.html': 'text/html; charset=utf-8',
  '.js': 'text/javascript; charset=utf-8',
  '.css': 'text/css; charset=utf-8',
  '.json': 'application/json; charset=utf-8',
  '.svg': 'image/svg+xml',
  '.png': 'image/png',
  '.ico': 'image/x-icon',
  '.webp': 'image/webp',
  '.woff2': 'font/woff2',
  '.map': 'application/json; charset=utf-8',
};

function setSecurityHeaders(res: ServerResponse): void {
  res.setHeader('x-content-type-options', 'nosniff');
  res.setHeader('referrer-policy', 'no-referrer');
  res.setHeader(
    'content-security-policy',
    "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'",
  );
}

function sendJSON(res: ServerResponse, status: number, body: unknown): void {
  setSecurityHeaders(res);
  res.writeHead(status, {
    'content-type': 'application/json; charset=utf-8',
    'cache-control': 'no-store',
  });
  res.end(JSON.stringify(body));
}

async function readJSON(req: IncomingMessage, limit = 64 * 1024): Promise<unknown> {
  const chunks: Buffer[] = [];
  let size = 0;
  for await (const chunk of req) {
    const buf = chunk as Buffer;
    size += buf.length;
    if (size > limit) throw new HttpError(413, '请求体过大');
    chunks.push(buf);
  }
  if (size === 0) throw new HttpError(400, '请求体为空');
  try {
    return JSON.parse(Buffer.concat(chunks).toString('utf8')) as unknown;
  } catch {
    throw new HttpError(400, '请求体不是合法 JSON');
  }
}

// mapCenterError 把中心侧的失败映射为 HTTP 语义：
// 未连接 = 503（面板可用但数据源不可达）；中心明确拒绝（RemoteError）= 400；其余（超时/传输）= 502。
function mapCenterError(err: unknown): HttpError {
  if (err instanceof RemoteError) return new HttpError(400, err.message);
  const msg = (err as Error).message;
  if (msg === '未连接到设置中心') return new HttpError(503, msg);
  // 目标不在名册 = 对端不可达，语义同 503（而不是 502 的"上游坏了"）。
  if (msg.startsWith('未知目标服务')) return new HttpError(503, msg);
  return new HttpError(502, msg);
}

function asObject(v: unknown): Record<string, unknown> | null {
  return v !== null && typeof v === 'object' ? (v as Record<string, unknown>) : null;
}

function requiredString(body: Record<string, unknown>, field: string): string {
  const v = body[field];
  if (typeof v !== 'string' || v.trim() === '') {
    throw new HttpError(400, `${field} 必填（非空字符串）`);
  }
  return v;
}

export function createRequestHandler(
  center: CenterLink,
  cfg: ApiConfig,
  log: (m: string) => void,
): (req: IncomingMessage, res: ServerResponse) => void {
  const overview = async (): Promise<unknown> => {
    let settings: SettingsResult | null = null;
    let error: string | undefined;
    if (center.connected) {
      try {
        settings = (await center.call('list_settings', {})) as SettingsResult;
      } catch (err) {
        error = (err as Error).message;
      }
    } else {
      error = '未连接到设置中心';
    }
    return {
      connected: center.connected,
      center: cfg.centerURL,
      self: { name: cfg.name, ws: cfg.selfURL, http: cfg.httpAddr },
      roster: center.roster,
      settings,
      error,
      fetchedAt: new Date().toISOString(),
    };
  };

  const mutate = async (res: ServerResponse, action: string, body: Record<string, unknown>): Promise<void> => {
    const service = requiredString(body, 'service');
    const key = requiredString(body, 'key');
    const input =
      action === 'set_setting'
        ? { service, key, value: requiredString(body, 'value') }
        : { service, key };
    let data: SettingsResult;
    try {
      data = (await center.call(action, input)) as SettingsResult;
    } catch (err) {
      throw mapCenterError(err);
    }
    const services: ServiceSettings[] = data?.services ?? [];
    sendJSON(res, 200, { ok: true, services });
  };

  // sendUserMessage 是门户的"用户 → 智能体"入口：跳 user_message 给 reasoner。
  // 返回的只是**受理回执**——最终回答由 reasoner 经 deliver 送回来，走 SSE，不从这条 HTTP 出。
  const sendUserMessage = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req)) ?? {};
    const text = requiredString(body, 'text');
    try {
      await center.jumpTo(cfg.reasoner, 'user_message', { text, conversation_id: PORTAL_CONVERSATION_ID });
    } catch (err) {
      throw mapCenterError(err);
    }
    sendJSON(res, 200, { ok: true, conversationId: PORTAL_CONVERSATION_ID });
  };

  // fetchHistory 把该会话最近的对话从 memory 取回来（门户启动时回填界面用）。
  // 与 user_message 同一条链路：从名册解析 memory 的地址后直连，中心不转发。
  const fetchHistory = async (res: ServerResponse, limit: number): Promise<void> => {
    let data: { messages?: unknown } | undefined;
    try {
      data = (await center.jumpTo(cfg.memory, 'history', {
        conversation_id: PORTAL_CONVERSATION_ID,
        limit,
      })) as { messages?: unknown };
    } catch (err) {
      throw mapCenterError(err);
    }
    const messages = Array.isArray(data?.messages) ? data.messages : [];
    sendJSON(res, 200, { conversationId: PORTAL_CONVERSATION_ID, messages });
  };

  // decideApproval 把用户的裁决送回 approval。裁决权在用户手里，门户只是搬运工：  // 这里不判断该不该批准，也不缓存结果——approval 认不认（待审项还在不在）由它自己决定。
  const decideApproval = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req)) ?? {};
    const itemId = requiredString(body, 'item_id');
    const decision = requiredString(body, 'decision');
    if (decision !== 'allow' && decision !== 'deny') throw new HttpError(400, 'decision 只能是 allow 或 deny');
    const note = typeof body.note === 'string' ? body.note : '';
    try {
      await center.jumpTo(cfg.approval, 'approval_decide', { item_id: itemId, decision, note });
    } catch (err) {
      throw mapCenterError(err);
    }
    cfg.approvals.remove(itemId);
    sendJSON(res, 200, { ok: true, itemId, decision });
  };

  const events = (req: IncomingMessage, res: ServerResponse): void => {
    setSecurityHeaders(res);
    res.writeHead(200, {
      'content-type': 'text/event-stream; charset=utf-8',
      'cache-control': 'no-store',
      connection: 'keep-alive',
    });
    const send = (event: string, data: unknown): void => {
      res.write(`event: ${event}\ndata: ${JSON.stringify(data)}\n\n`);
    };
    send('status', { connected: center.connected, center: cfg.centerURL });
    send('roster', center.roster ?? { services: [], revision: 0 });
    send('timers', cfg.timers.current()); // 冷启动先补一次当前快照（浏览器新开标签也能立刻看到倒计时）
    // 待审项同理：新标签打开就能看到在等的审批。没有就不发（不制造空事件噪音）。
    const pendingApprovals = cfg.approvals.current();
    if (pendingApprovals.length > 0) send('approvals', pendingApprovals);
    const unsubscribe = center.on((e: CenterEvent) => {
      if (e.kind === 'status') send('status', { connected: e.connected, center: cfg.centerURL });
      else if (e.kind === 'roster') send('roster', e.roster as RosterPayload);
      else send('settings', e.event);
    });
    const unsubscribeDeliver = cfg.portal.on((m: DeliverMessage) => {
      send('deliver', m);
    });
    const unsubscribeTimers = cfg.timers.on((items) => {
      send('timers', items);
    });
    const unsubscribeApprovals = cfg.approvals.on((items) => {
      send('approvals', items);
    });
    const keepalive = setInterval(() => {
      res.write(': ping\n\n');
    }, 20_000);
    req.on('close', () => {
      clearInterval(keepalive);
      unsubscribe();
      unsubscribeDeliver();
      unsubscribeTimers();
      unsubscribeApprovals();
    });
  };

  const statOrNull = async (p: string): Promise<Awaited<ReturnType<typeof stat>> | null> => {
    try {
      return await stat(p);
    } catch {
      return null;
    }
  };

  const serveStatic = async (res: ServerResponse, pathname: string): Promise<void> => {
    const webRoot = resolve(cfg.webDir);
    const filePath = resolve(join(webRoot, pathname));
    if (filePath !== webRoot && !filePath.startsWith(webRoot + sep)) {
      throw new HttpError(403, '非法路径');
    }
    let target = filePath;
    let info = await statOrNull(target);
    if (info?.isDirectory()) {
      target = join(target, 'index.html');
      info = await statOrNull(target);
    }
    if (!info) {
      if (extname(pathname) !== '') throw new HttpError(404, '资源不存在');
      target = join(webRoot, 'index.html');
      info = await statOrNull(target);
      if (!info) throw new HttpError(503, '前端未构建（dist/web 缺失）——先执行 npm run build');
    }
    setSecurityHeaders(res);
    res.writeHead(200, {
      'content-type': contentTypes[extname(target).toLowerCase()] ?? 'application/octet-stream',
      'content-length': String(info.size),
      'cache-control': pathname.startsWith('/assets/') ? 'public, max-age=31536000, immutable' : 'no-cache',
    });
    await new Promise<void>((resolvePipe, rejectPipe) => {
      const stream = createReadStream(target);
      stream.on('error', rejectPipe);
      stream.on('end', resolvePipe);
      stream.pipe(res);
    });
  };

  // ---- 语言包：启动语言 + 目录扫描（浏览器切语言时按需读，不重启服务）----
  interface LangPack {
    locale: string;
    name: string;
    messages: Record<string, string>;
  }
  const packPath = (locale: string): string => join(cfg.localesDir, `${locale}.json`);
  const loadPack = async (locale: string): Promise<LangPack> => {
    // 只认 [A-Za-z0-9_-]，避免 ../ 之类的路径穿越
    if (!/^[A-Za-z0-9_-]+$/.test(locale)) throw new HttpError(400, `语言标识不合法: ${locale}`);
    try {
      const raw = await readFile(packPath(locale), 'utf8');
      const parsed = JSON.parse(raw) as { locale?: string; name?: string; messages?: Record<string, string> };
      return { locale: parsed.locale ?? locale, name: parsed.name ?? locale, messages: parsed.messages ?? {} };
    } catch {
      // 缺包不算致命：返回空，界面各处会退回服务声明/内置中文
      return { locale, name: locale, messages: {} };
    }
  };
  const listLocales = async (): Promise<{ locale: string; name: string }[]> => {
    try {
      const files = await readdir(cfg.localesDir);
      const out: { locale: string; name: string }[] = [];
      for (const f of files) {
        if (!f.endsWith('.json')) continue;
        const locale = f.slice(0, -5);
        if (!/^[A-Za-z0-9_-]+$/.test(locale)) continue;
        const pack = await loadPack(locale);
        out.push({ locale: pack.locale, name: pack.name });
      }
      return out;
    } catch {
      return [];
    }
  };

  const route = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    // 信任栅栏先于一切路由：Host 是 DNS 重绑伪造不了的那个头，Origin 挡跨站请求。
    // 门户能驱动智能体（而智能体手里有 shell），所以这道栅栏不是可选项。
    const denied = checkRequestTrust(req.headers.host, req.headers.origin, cfg.trust);
    if (denied !== null) throw new HttpError(403, denied);
    const url = new URL(req.url ?? '/', 'http://localhost');
    const pathname = decodeURIComponent(url.pathname);

    if (pathname === '/api/overview' && req.method === 'GET') {
      sendJSON(res, 200, await overview());
      return;
    }
    if (pathname === '/api/message' && req.method === 'POST') {
      await sendUserMessage(req, res);
      return;
    }
    if (pathname === '/api/settings/set' && req.method === 'POST') {
      await mutate(res, 'set_setting', asObject(await readJSON(req)) ?? {});
      return;
    }
    if (pathname === '/api/settings/reset' && req.method === 'POST') {
      await mutate(res, 'reset_setting', asObject(await readJSON(req)) ?? {});
      return;
    }
    if (pathname === '/api/events' && req.method === 'GET') {
      events(req, res);
      return;
    }
    if (pathname === '/api/i18n/locales' && req.method === 'GET') {
      sendJSON(res, 200, { locales: await listLocales() });
      return;
    }
    if (pathname === '/api/i18n' && req.method === 'GET') {
      const want = url.searchParams.get('lang') ?? cfg.lang;
      sendJSON(res, 200, await loadPack(want));
      return;
    }
    if (pathname === '/api/history' && req.method === 'GET') {
      const raw = Number(url.searchParams.get('limit') ?? '');
      const limit = Number.isFinite(raw) && raw > 0 ? Math.min(Math.trunc(raw), 500) : 50;
      await fetchHistory(res, limit);
      return;
    }
    if (pathname === '/api/approvals' && req.method === 'GET') {
      sendJSON(res, 200, { items: cfg.approvals.current() });
      return;
    }
    if (pathname === '/api/approvals' && req.method === 'POST') {
      await decideApproval(req, res);
      return;
    }
    if (pathname === '/api/overview' || pathname === '/api/message' || pathname === '/api/settings/set' || pathname === '/api/settings/reset' || pathname === '/api/events' || pathname === '/api/approvals' || pathname === '/api/history' || pathname === '/api/i18n' || pathname === '/api/i18n/locales') {
      throw new HttpError(405, `方法不允许: ${req.method ?? ''}`);
    }
    if (pathname.startsWith('/api/')) {
      throw new HttpError(404, `未知接口: ${pathname}`);
    }
    await serveStatic(res, pathname);
  };

  return (req, res) => {
    route(req, res).catch((err: unknown) => {
      if (res.headersSent) {
        res.end();
        return;
      }
      if (err instanceof HttpError) {
        sendJSON(res, err.status, { error: err.message });
        return;
      }
      log(`请求处理失败: ${(err as Error).message}`);
      sendJSON(res, 500, { error: '内部错误' });
    });
  };
}
