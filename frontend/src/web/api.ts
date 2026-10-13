// 与前端服务 HTTP API 的通信（JSON 进出；失败统一抛 Error，消息即中文提示）。
import type { Overview, ServiceSettingsView } from './types';

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
