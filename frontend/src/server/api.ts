// 面板的 HTTP 层：浏览器 JSON API + SSE 事件流 + 静态资源。
// 只做“翻译”：把 HTTP 请求翻成中心跳转，把回执翻回 JSON——不缓存业务数据、不落盘。
import { createReadStream } from 'node:fs';
import { stat } from 'node:fs/promises';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { extname, join, resolve, sep } from 'node:path';
import type { CenterEvent, CenterLink } from './center.ts';
import { RemoteError } from './center.ts';
import type { RosterPayload, ServiceSettings, SettingsResult } from './protocol.ts';

export interface ApiConfig {
  name: string;
  httpAddr: string;
  selfURL: string;
  centerURL: string;
  webDir: string;
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
    const unsubscribe = center.on((e: CenterEvent) => {
      if (e.kind === 'status') send('status', { connected: e.connected, center: cfg.centerURL });
      else send('roster', e.roster as RosterPayload);
    });
    const keepalive = setInterval(() => {
      res.write(': ping\n\n');
    }, 20_000);
    req.on('close', () => {
      clearInterval(keepalive);
      unsubscribe();
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

  const route = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const url = new URL(req.url ?? '/', 'http://localhost');
    const pathname = decodeURIComponent(url.pathname);

    if (pathname === '/api/overview' && req.method === 'GET') {
      sendJSON(res, 200, await overview());
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
    if (pathname === '/api/overview' || pathname === '/api/settings/set' || pathname === '/api/settings/reset' || pathname === '/api/events') {
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
