// 与前端服务 HTTP API 的通信（JSON 进出；失败统一抛 Error，消息即中文提示）。
import type { ConversationView, Overview, PortalMessage, ServiceSettingsView, TaskView, TraceMeta } from './types';

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, init);
  let body: unknown = null;
  try {
    body = await res.json();
  } catch {
    // 非 JSON 响应（理论上不该出现）
  }
  if (!res.ok) {
    const fromBody = body !== null && typeof body === 'object' ? (body as { error?: unknown }).error : undefined;
    throw new Error(typeof fromBody === 'string' ? fromBody : `HTTP ${res.status}`);
  }
  return body as T;
}

export function fetchOverview(): Promise<Overview> {
  return request<Overview>('/api/overview');
}

// fetchHistory 取某个任务的记录（门户回填；真相在 memory 服务，门户不自己存）。
// taskId 为空 = 当前任务（由记忆服务决定）。返回按 seq/落库顺序排好的完整条目（含思考与工具轨迹）。
export async function fetchHistory(
  limit = 100,
  afterSeq = 0,
  taskId = '',
  all = false,
  conversationId = '',
): Promise<PortalMessage[]> {
  const cid = conversationId === '' ? '' : `&conversation_id=${encodeURIComponent(conversationId)}`;
  const q = `${afterSeq > 0 ? `&after_seq=${String(afterSeq)}` : ''}${taskId === '' ? '' : `&task_id=${encodeURIComponent(taskId)}`}${all ? '&all=1' : ''}${cid}`;
  const r = await request<{
    messages?: {
      id?: number;
      seq?: number;
      role?: string;
      kind?: string;
      content?: string;
      meta?: string;
      at?: string;
      task_id?: string;
    }[];
  }>(`/api/history?limit=${String(limit)}${q}`);
  const out: PortalMessage[] = [];
  for (const m of r.messages ?? []) {
    const message = historyToMessage(m);
    if (message !== null) out.push(message);
  }
  return out;
}

// fetchTasks 取任务列表。面板是从上到下的会话，侧栏是它的目录：同样按时间正序，
// 读起来才对得上（服务端给的是"最近活跃在前"，这里翻过来）。
export async function fetchTasks(conversationId = ''): Promise<TaskView[]> {
  const q = conversationId === '' ? '' : `?conversation_id=${encodeURIComponent(conversationId)}`;
  const r = await request<{ tasks?: TaskView[] }>(`/api/tasks${q}`);
  return [...(r.tasks ?? [])].sort((a, b) => a.created_at.localeCompare(b.created_at));
}

// ---- 会话（门户的第一层）----

export async function fetchConversations(includeArchived = false): Promise<ConversationView[]> {
  const q = includeArchived ? '?include_archived=1' : '';
  const r = await request<{ conversations?: ConversationView[] }>(`/api/conversations${q}`);
  return r.conversations ?? [];
}

export async function createConversation(title = ''): Promise<ConversationView> {
  const r = await request<{ conversation?: ConversationView }>('/api/conversations', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ title }),
  });
  if (r.conversation === undefined) throw new Error('新建会话没有返回会话');
  return r.conversation;
}

export async function renameConversation(conversationId: string, title: string): Promise<void> {
  await request<{ conversation?: ConversationView }>('/api/conversations/rename', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ conversation_id: conversationId, title }),
  });
}

export async function archiveConversation(conversationId: string, archived = true): Promise<void> {
  await request<{ conversation?: ConversationView }>('/api/conversations/archive', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ conversation_id: conversationId, archived }),
  });
}

// historyToMessage 把一条落库记录翻成界面消息：对话消息进气泡，思考/命令/输出进轨迹条目。
export function historyToMessage(m: {
  id?: number;
  seq?: number;
  role?: string;
  kind?: string;
  content?: string;
  meta?: string;
  at?: string;
  task_id?: string;
}): PortalMessage | null {
  const kind = m.kind ?? (m.role === 'user' ? 'user' : 'assistant');
  const text = m.content ?? '';
  const at = m.at ?? '';
  const seq = m.seq ?? 0;
  const taskId = m.task_id ?? '';
  if (kind === 'user') return { author: 'user', kind: 'user', text, at, seq, taskId };
  if (kind === 'assistant') return { author: 'agent', kind: 'assistant', text, at, seq, taskId };
  if (kind === 'reasoning' || kind === 'tool_call' || kind === 'tool_result') {
    return { author: 'agent', kind, text, at, seq, taskId, meta: parseMeta(m.meta) };
  }
  return null; // 其他 role（system 等）不进对话视图
}

// parseMeta 解析落库时带的附加信息（形状不对就当没有——展示层不该因为脏数据崩）。
export function parseMeta(raw: string | undefined): TraceMeta | undefined {
  if (raw === undefined || raw === '') return undefined;
  try {
    const v = JSON.parse(raw) as Record<string, unknown>;
    const meta: TraceMeta = {};
    if (typeof v.tool === 'string') meta.tool = v.tool;
    if (typeof v.call_id === 'string') meta.callId = v.call_id;
    if (v.truncated === true) meta.truncated = true;
    if (typeof v.full_chars === 'number') meta.fullChars = v.full_chars;
    return meta;
  } catch {
    return undefined;
  }
}

export async function setSetting(service: string, key: string, value: string): Promise<ServiceSettingsView[]> {
  const r = await request<{ services?: ServiceSettingsView[] }>('/api/settings/set', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ service, key, value }),
  });
  return r.services ?? [];
}

export async function resetSetting(service: string, key: string): Promise<ServiceSettingsView[]> {
  const r = await request<{ services?: ServiceSettingsView[] }>('/api/settings/reset', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ service, key }),
  });
  return r.services ?? [];
}

/** 附件描述：/api/upload 的返回值，发送消息时原样带回。 */
export interface UploadResult {
  kind: 'image' | 'file';
  id: string;
  filename: string;
  mime: string;
  size: number;
}

// uploadFile 把浏览器选的文件交给门户转存（门户 → files/images，不经过模型）。
// 用 JSON + base64：与其余接口同一条路，保住"只收 application/json"这道跨站闸。
export async function uploadFile(file: File): Promise<UploadResult> {
  const data = await readAsBase64(file);
  return request<UploadResult>('/api/upload', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ filename: file.name, mime: file.type, data }),
  });
}

// readAsBase64 读成不带 data: 前缀的标准 base64（门户只认这个形状）。
function readAsBase64(file: File): Promise<string> {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => { reject(new Error('读取文件失败')); };
    reader.onload = () => {
      const result = typeof reader.result === 'string' ? reader.result : '';
      const comma = result.indexOf(',');
      resolve(comma >= 0 ? result.slice(comma + 1) : result);
    };
    reader.readAsDataURL(file);
  });
}

// runTerminalCommand 让门户把命令交给 shell（boss=门户）。返回的只是**受理回执**：
// 执行与审批都在后台，输出经 SSE 'terminal' 事件回来（按 itemId 对应）。
export function runTerminalCommand(command: string, conversationId = ''): Promise<{ itemId: string; phase: string }> {
  return request<{ itemId: string; phase: string }>('/api/terminal', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(conversationId === '' ? { command } : { command, conversation_id: conversationId }),
  });
}

// sendMessage 把用户消息交给门户——只等**受理回执**，最终回答由 deliver 经 SSE 送回来。
export function sendMessage(
  text: string,
  attachments: UploadResult[] = [],
  conversationId = '',
): Promise<{ conversationId: string }> {
  const body: Record<string, unknown> = conversationId === '' ? { text } : { text, conversation_id: conversationId };
  if (attachments.length > 0) {
    body.attachments = attachments.map((a) => ({
      kind: a.kind,
      id: a.id,
      filename: a.filename,
      size: a.size,
    }));
  }
  return request<{ conversationId: string }>('/api/message', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// resumeTask 恢复一个未完结任务：记忆服务把它顶成当前任务，并把这一段记录带回来。
// 已完结的任务由记忆服务明确拒绝——门户不自己判断能不能继续。
export async function resumeTask(taskId: string): Promise<PortalMessage[]> {
  const r = await request<{
    task?: unknown;
    messages?: {
      id?: number;
      seq?: number;
      role?: string;
      kind?: string;
      content?: string;
      meta?: string;
      at?: string;
      task_id?: string;
    }[];
  }>('/api/tasks/resume', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ task_id: taskId }),
  });
  const out: PortalMessage[] = [];
  for (const m of r.messages ?? []) {
    const message = historyToMessage(m);
    if (message !== null) out.push(message);
  }
  return out;
}

// decideApproval 把用户的裁决送回 approval（allow = 批准执行，deny = 拒绝）。
export function decideApproval(itemId: string, decision: 'allow' | 'deny', note = ''): Promise<{ itemId: string }> {
  return request<{ itemId: string }>('/api/approvals', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ item_id: itemId, decision, note }),
  });
}

// retryRound 让 reasoner 对「上一轮失败、任务还开着」的会话再跑一轮。
// 能不能重试由 reasoner 判定（门户不猜）：被拒时会抛出带原因的错误。
export function retryRound(conversationId = ''): Promise<{ retried: boolean }> {
  return request<{ retried: boolean }>('/api/retry', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(conversationId === '' ? {} : { conversation_id: conversationId }),
  });
}