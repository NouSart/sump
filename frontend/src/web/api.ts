// 与前端服务 HTTP API 的通信（JSON 进出；失败统一抛 Error，消息即中文提示）。
import type { Overview, PortalMessage, ServiceSettingsView, TaskView, TraceMeta } from './types';

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
export async function fetchHistory(limit = 100, afterSeq = 0, taskId = '', all = false): Promise<PortalMessage[]> {
  const q = `${afterSeq > 0 ? `&after_seq=${String(afterSeq)}` : ''}${taskId === '' ? '' : `&task_id=${encodeURIComponent(taskId)}`}${all ? '&all=1' : ''}`;
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
export async function fetchTasks(): Promise<TaskView[]> {
  const r = await request<{ tasks?: TaskView[] }>('/api/tasks');
  return [...(r.tasks ?? [])].sort((a, b) => a.created_at.localeCompare(b.created_at));
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

// sendMessage 把用户消息交给门户——只等**受理回执**，最终回答由 deliver 经 SSE 送回来。
export function sendMessage(text: string): Promise<{ conversationId: string }> {
  return request<{ conversationId: string }>('/api/message', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ text }),
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
