// 与前端服务 HTTP API 的通信（JSON 进出；失败统一抛 Error，消息即中文提示）。
import type { Overview, PortalMessage, ServiceSettingsView } from './types';

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

// fetchHistory 取该会话最近的对话（门户启动时回填；真相在 memory 服务，门户不自己存）。
export async function fetchHistory(limit = 50): Promise<PortalMessage[]> {
  const r = await request<{ messages?: { role?: string; content?: string; at?: string }[] }>(
    `/api/history?limit=${String(limit)}`,
  );
  const out: PortalMessage[] = [];
  for (const m of r.messages ?? []) {
    if (m.role !== 'user' && m.role !== 'assistant') continue; // tool/system 不进对话视图
    out.push({ author: m.role === 'user' ? 'user' : 'agent', text: m.content ?? '', at: m.at ?? '' });
  }
  return out;
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

// decideApproval 把用户的裁决送回 approval（allow = 批准执行，deny = 拒绝）。
export function decideApproval(itemId: string, decision: 'allow' | 'deny', note = ''): Promise<{ itemId: string }> {
  return request<{ itemId: string }>('/api/approvals', {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ item_id: itemId, decision, note }),
  });
}
