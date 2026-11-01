// 面板的 HTTP 层：浏览器 JSON API + SSE 事件流 + 静态资源。
// 只做“翻译”：把 HTTP 请求翻成中心跳转，把回执翻回 JSON——不缓存业务数据、不落盘。
import { createReadStream } from 'node:fs';
import { readdir, readFile } from 'node:fs/promises';
import { stat } from 'node:fs/promises';
import type { IncomingMessage, ServerResponse } from 'node:http';
import { extname, join, resolve, sep } from 'node:path';
import type { CenterEvent, CenterLink } from './center.ts';
import { RemoteError } from './center.ts';
import { PORTAL_CONVERSATION_ID, checkRequestTrust, type DeliverMessage, type PendingApprovalItem, type ProgressUpdate, type TerminalUpdate, type TimerItem, type TrustConfig } from './portal.ts';
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
  /** 任务实况订阅（progress → SSE）：思考 / 命令 / 输出，进行中就能看见。 */
  progress: { on: (fn: (p: ProgressUpdate) => void) => () => void };
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
  /** 文件服务名：上传的文档转交给它（模型用 files.read 出正文）。 */
  files: string;
  /** 图片服务名：上传的图片转交给它（模型用 images.fetch 看缩略图 / 局部）。 */
  images: string;
  /** 门户自己的上传上限（字节）：比服务侧权威上限更严，免得把超大文件编码成 JSON 再打进来。 */
  uploadMaxBytes: number;
  /** shell 服务名：终端命令经它执行（仍走审批）。 */
  shell: string;
  /** 终端事件订阅（report_result → SSE）；recent 给新连接补发最近几条。 */
  terminal: { recent: () => TerminalUpdate[]; on: (fn: (u: TerminalUpdate) => void) => () => void };
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
  // 只收 application/json：跨站表单 / text/plain 简单请求不带预检，
  // 浏览器会直接把请求打进来——限定 Content-Type 后跨站必须先过预检，
  // 与信任栅栏形成两道独立的闸。
  const contentType = String(req.headers['content-type'] ?? '').toLowerCase();
  if (!contentType.startsWith('application/json')) {
    throw new HttpError(415, '请求体必须是 application/json');
  }
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

/** 上传附件的描述：浏览器上传成功后拿到，发送消息时原样带回来。 */
interface AttachmentRef {
  kind: 'image' | 'file';
  id: string;
  filename: string;
  size: number;
}

const attachmentIdPattern = /^[A-Za-z0-9_-]{1,64}$/;

// sanitizeFilename 去掉控制字符：文件名会进消息正文，换行能伪造出第二条"附件"行。
function sanitizeFilename(name: string): string {
  const cleaned = name.replace(/[\r\n\t\u0000-\u001f]/g, ' ').trim();
  return cleaned.length > 200 ? `${cleaned.slice(0, 200)}…` : cleaned;
}

// parseAttachments 校验浏览器回传的附件清单：形状不对就整批拒（不猜、不静默丢）。
function parseAttachments(raw: unknown): AttachmentRef[] {
  if (raw === undefined || raw === null) return [];
  if (!Array.isArray(raw)) throw new HttpError(400, 'attachments 必须是数组');
  if (raw.length > 10) throw new HttpError(400, 'attachments 最多 10 个');
  return raw.map((x, i) => {
    const o = asObject(x);
    const kind = o?.kind;
    if (kind !== 'image' && kind !== 'file') {
      throw new HttpError(400, `attachments[${i}].kind 必须是 image 或 file`);
    }
    const id = typeof o?.id === 'string' ? o.id.trim() : '';
    if (!attachmentIdPattern.test(id)) throw new HttpError(400, `attachments[${i}].id 不合法`);
    const rawName = typeof o?.filename === 'string' ? sanitizeFilename(o.filename) : '';
    const size = typeof o?.size === 'number' && Number.isFinite(o.size) && o.size >= 0 ? Math.trunc(o.size) : 0;
    return { kind, id, filename: rawName === '' ? id : rawName, size };
  });
}

// attachmentLine 把附件写成模型看得懂的一行：动作名与 id 都给全，省得它去猜怎么取。
function attachmentLine(a: AttachmentRef): string {
  const size = a.size > 0 ? `，${humanBytes(a.size)}` : '';
  return a.kind === 'image'
    ? `[附件] ${a.filename}（images:${a.id}${size}）——用 images.fetch {id:"${a.id}"} 看；要看细节就给 x1,y1,x2,y2 取那一块原图`
    : `[附件] ${a.filename}（files:${a.id}${size}）——需要时用 files.read {id:"${a.id}"} 读`;
}

// base64Size 校验 base64 并返回解码后的字节数：坏数据在这里挡住，不转给下游。
function base64Size(data: string): number {
  const trimmed = data.trim();
  if (trimmed === '' || trimmed.length % 4 !== 0 || !/^[A-Za-z0-9+/]+={0,2}$/.test(trimmed)) {
    throw new HttpError(400, 'data 必须是标准 base64（不要带 data: 前缀）');
  }
  const bytes = Buffer.from(trimmed, 'base64');
  if (bytes.length === 0) throw new HttpError(400, 'data 解码后为空');
  return bytes.length;
}

function humanBytes(n: number): string {
  if (n < 1024) return `${n} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

export function createRequestHandler(
  center: CenterLink,
  cfg: ApiConfig,
  log: (m: string) => void,
): (req: IncomingMessage, res: ServerResponse) => void {
  // 事件流上限：每个标签页占一条长连接、四组订阅；不设上限时一处恶意页面
  //（或失控的客户端）就能把进程的句柄与内存吃光。慢消费者超过缓冲上限直接断开，
  // 让它重连后拿全量快照，而不是把内存当队列。
  const maxEventStreams = 32;
  const maxEventStreamBuffer = 8 << 20;
  let eventStreams = 0;

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
  //
  // 附件不进协议：门户把每个附件拼成一行正文（带动作名与 id），模型照常自己调
  // files.read / images.fetch。协议只增不改，等真有必要再给 user_message 加字段。
  const sendUserMessage = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req)) ?? {};
    const rawText = typeof body.text === 'string' ? body.text.trim() : '';
    const attachments = parseAttachments(body.attachments);
    if (rawText === '' && attachments.length === 0) {
      throw new HttpError(400, 'text 必填（或有附件）');
    }
    const text = [rawText, ...attachments.map((a) => attachmentLine(a))].filter((s) => s !== '').join('\n\n');
    try {
      await center.jumpTo(cfg.reasoner, 'user_message', { text, conversation_id: PORTAL_CONVERSATION_ID });
    } catch (err) {
      throw mapCenterError(err);
    }
    sendJSON(res, 200, { ok: true, conversationId: PORTAL_CONVERSATION_ID });
  };

  // upload 把浏览器选的文件转交给文件 / 图片服务。
  //
  // 门户只做"过路"：校验形状与**门户自己的**上限（20 MiB），按 mime 分流，不落盘、不缓存。
  // 图片进 images（模型能用缩略图 / 矩形窗口看局部），其余进 files（模型用 read 出正文）。
  // 服务侧的权威上限（files 100 MiB / images 32 MiB）不在这里重复实现，拒了就把它的人话转发。
  const uploadBodyLimit = Math.ceil((cfg.uploadMaxBytes * 4) / 3) + 4 * 1024;
  const upload = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req, uploadBodyLimit)) ?? {};
    const filename = sanitizeFilename(requiredString(body, 'filename'));
    const data = requiredString(body, 'data');
    const mime = typeof body.mime === 'string' ? body.mime.trim() : '';
    const size = base64Size(data);
    if (size > cfg.uploadMaxBytes) {
      throw new HttpError(
        413,
        `${filename} 有 ${humanBytes(size)}，超过门户上限 ${humanBytes(cfg.uploadMaxBytes)}（files/images 自己的上限更宽，是门户先挡）`,
      );
    }
    const isImage = mime.startsWith('image/');
    const target = isImage ? cfg.images : cfg.files;
    let saved: { id?: unknown; size?: unknown; mime?: unknown } | undefined;
    try {
      saved = (await center.jumpTo(
        target,
        'save',
        isImage ? { data, mime } : { data, filename, mime },
      )) as { id?: unknown; size?: unknown; mime?: unknown };
    } catch (err) {
      throw mapCenterError(err);
    }
    const id = typeof saved?.id === 'string' ? saved.id : '';
    if (id === '') {
      throw new HttpError(502, `${target} 没有回 id（保存结果不可用）`);
    }
    sendJSON(res, 200, {
      kind: isImage ? 'image' : 'file',
      id,
      filename,
      mime: typeof saved?.mime === 'string' && saved.mime !== '' ? saved.mime : mime,
      size: typeof saved?.size === 'number' ? saved.size : size,
    });
  };

  // runTerminalCommand：用户在终端里敲回车 → 门户直接把命令交给 shell。
  //
  // 审批不绕过：门户只是另一个调用方，shell 照样找 approval；待审项的 item_id 就是这里回的
  // itemId，所以终端能精确显示"等待审批"。命令本身不进对话（模型不需要为它花一轮推理），
  // 但执行完的结果由 app.ts 入账到会话记录（见 recordTerminal）。
  const runTerminalCommand = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req)) ?? {};
    const command = requiredString(body, 'command').trim();
    if (command.length > 16 * 1024) {
      throw new HttpError(400, '命令超过 16 KiB（shell 侧同样会拒）');
    }
    const workDir = typeof body.work_dir === 'string' ? body.work_dir.trim() : '';
    let receipt: { item_id?: unknown; phase?: unknown } | undefined;
    try {
      receipt = (await center.jumpTo(
        cfg.shell,
        'run',
        workDir === '' ? { command } : { command, work_dir: workDir },
      )) as { item_id?: unknown; phase?: unknown };
    } catch (err) {
      throw mapCenterError(err);
    }
    const itemId = typeof receipt?.item_id === 'string' ? receipt.item_id : '';
    if (itemId === '') {
      throw new HttpError(502, `${cfg.shell} 没有回 item_id（受理回执不可用）`);
    }
    sendJSON(res, 200, { itemId, phase: typeof receipt?.phase === 'string' ? receipt.phase : 'queued' });
  };

  // fetchHistory 把该会话最近的对话从 memory 取回来（门户启动时回填界面用）。
  // 与 user_message 同一条链路：从名册解析 memory 的地址后直连，中心不转发。
  const fetchTasks = async (res: ServerResponse): Promise<void> => {
    let data: { tasks?: unknown } | undefined;
    try {
      data = (await center.jumpTo(cfg.memory, 'task_list', {
        conversation_id: PORTAL_CONVERSATION_ID,
      })) as { tasks?: unknown };
    } catch (err) {
      throw mapCenterError(err);
    }
    const tasks = Array.isArray(data?.tasks) ? data.tasks : [];
    sendJSON(res, 200, { conversationId: PORTAL_CONVERSATION_ID, tasks });
  };

  // resumeTask 恢复一个未完结任务：把"当前任务"顶到它（下一条用户消息就落在这个任务里）。
  // 已完结的任务会被记忆服务明确拒绝——门户不自己判断任务能不能继续。
  const resumeTask = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req)) ?? {};
    const taskId = requiredString(body, 'task_id');
    let data: { task?: unknown; messages?: unknown } | undefined;
    try {
      data = (await center.jumpTo(cfg.memory, 'task_resume', { task_id: taskId })) as {
        task?: unknown;
        messages?: unknown;
      };
    } catch (err) {
      throw mapCenterError(err);
    }
    sendJSON(res, 200, {
      task: data?.task ?? null,
      messages: Array.isArray(data?.messages) ? data.messages : [],
    });
  };

  // retryRound 让 reasoner 对「上一轮失败、任务还开着」的会话再跑一轮。
  // 门户不自己判断能不能重试——把 conversation_id 交出去，由 reasoner 回答。
  const retryRound = async (req: IncomingMessage, res: ServerResponse): Promise<void> => {
    const body = asObject(await readJSON(req)) ?? {};
    const rawCid = typeof body.conversation_id === 'string' ? body.conversation_id.trim() : '';
    const conversationId = rawCid || PORTAL_CONVERSATION_ID;
    let data: { retried?: unknown } | undefined;
    try {
      data = (await center.jumpTo(cfg.reasoner, 'retry', { conversation_id: conversationId })) as {
        retried?: unknown;
      };
    } catch (err) {
      throw mapCenterError(err);
    }
    sendJSON(res, 200, { retried: data?.retried === true });
  };

  // fetchHistory 取某任务（默认当前任务）的记录：直连记忆服务，中心只当地址簿。
  const fetchHistory = async (
    res: ServerResponse,
    limit: number,
    afterSeq: number,
    taskId: string,
    all: boolean,
  ): Promise<void> => {
    let data: { messages?: unknown } | undefined;
    try {
      data = (await center.jumpTo(cfg.memory, 'history', {
        conversation_id: PORTAL_CONVERSATION_ID,
        limit,
        after_seq: afterSeq,
        ...(taskId === '' ? {} : { task_id: taskId }),
        ...(all ? { all_tasks: true } : {}),
      })) as { messages?: unknown };
    } catch (err) {
      throw mapCenterError(err);
    }
    const messages = Array.isArray(data?.messages) ? data.messages : [];
    sendJSON(res, 200, { conversationId: PORTAL_CONVERSATION_ID, messages });
  };

  // decideApproval 把用户的裁决送回 approval。裁决权在用户手里，门户只是搬运工：
  // 这里不判断该不该批准，也不缓存结果——approval 认不认（待审项还在不在）由它自己决定。
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
    if (eventStreams >= maxEventStreams) {
      throw new HttpError(503, '事件流连接数已满，请稍后再试');
    }
    eventStreams++;
    let released = false;
    const release = (): void => {
      if (released) return;
      released = true;
      eventStreams--;
    };
    setSecurityHeaders(res);
    res.writeHead(200, {
      'content-type': 'text/event-stream; charset=utf-8',
      'cache-control': 'no-store',
      connection: 'keep-alive',
    });
    const send = (event: string, data: unknown): void => {
      if (res.writableEnded) return;
      if (res.writableLength > maxEventStreamBuffer) {
        release();
        res.destroy();
        return;
      }
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
    const unsubscribeProgress = cfg.progress.on((p: ProgressUpdate) => {
      send('progress', p);
    });
    // 终端：先补最近几条（刷新页面还能看见刚跑完的输出），再跟着推变化。
    for (const u of cfg.terminal.recent()) send('terminal', u);
    const unsubscribeTerminal = cfg.terminal.on((u: TerminalUpdate) => {
      send('terminal', u);
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
      release();
      unsubscribe();
      unsubscribeDeliver();
      unsubscribeProgress();
      unsubscribeTerminal();
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
    if (pathname === '/api/upload' && req.method === 'POST') {
      await upload(req, res);
      return;
    }
    if (pathname === '/api/terminal' && req.method === 'POST') {
      await runTerminalCommand(req, res);
      return;
    }
    if (pathname === '/api/retry' && req.method === 'POST') {
      await retryRound(req, res);
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
      const limit = Number.isFinite(raw) && raw > 0 ? Math.min(Math.trunc(raw), 500) : 100;
      const rawAfter = Number(url.searchParams.get('after_seq') ?? '');
      const afterSeq = Number.isFinite(rawAfter) && rawAfter > 0 ? Math.trunc(rawAfter) : 0;
      await fetchHistory(res, limit, afterSeq, url.searchParams.get('task_id') ?? '', url.searchParams.get('all') === '1');
      return;
    }
    if (pathname === '/api/tasks' && req.method === 'GET') {
      await fetchTasks(res);
      return;
    }
    if (pathname === '/api/tasks/resume' && req.method === 'POST') {
      await resumeTask(req, res);
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
    if (pathname === '/api/overview' || pathname === '/api/message' || pathname === '/api/upload' || pathname === '/api/terminal' || pathname === '/api/retry' || pathname === '/api/settings/set' || pathname === '/api/settings/reset' || pathname === '/api/events' || pathname === '/api/approvals' || pathname === '/api/history' || pathname === '/api/i18n' || pathname === '/api/i18n/locales') {
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
