// 组件测试的替身设施：假 EventSource（真实 SSE 的 JSON 载荷语义）、fetch 路由桩、
// 数据模板（Overview / 名册 / 设置）。
import { vi } from 'vitest';
import type { Overview, RosterView, ServiceSettingsView } from '../../src/web/types';

// FakeEventSource 模拟浏览器的 EventSource：记录实例，emit 触发已注册的监听器。
export class FakeEventSource {
  static instances: FakeEventSource[] = [];

  readonly url: string;
  readyState = 1;
  private readonly handlers = new Map<string, ((ev: MessageEvent) => void)[]>();

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, fn: (ev: MessageEvent) => void): void {
    const list = this.handlers.get(type) ?? [];
    list.push(fn);
    this.handlers.set(type, list);
  }

  removeEventListener(type: string, fn: (ev: MessageEvent) => void): void {
    this.handlers.set(type, (this.handlers.get(type) ?? []).filter((f) => f !== fn));
  }

  close(): void {
    this.readyState = 2;
  }

  // emit 触发一个 SSE 事件（data JSON 序列化——与真实 SSE 的 MessageEvent.data 一致）。
  emit(type: string, data: unknown): void {
    for (const fn of this.handlers.get(type) ?? []) {
      fn({ data: JSON.stringify(data) } as MessageEvent);
    }
  }
}

// installFakeEventSource 替换全局 EventSource（重置实例表）。
export function installFakeEventSource(): void {
  FakeEventSource.instances = [];
  vi.stubGlobal('EventSource', FakeEventSource as unknown as typeof EventSource);
}

// latestSource 返回最近创建的假 EventSource（App 在挂载时创建）。
export function latestSource(): FakeEventSource {
  const es = FakeEventSource.instances.at(-1);
  if (!es) throw new Error('尚未创建 EventSource');
  return es;
}

export interface FetchCall {
  url: string;
  init: RequestInit | undefined;
}

export function jsonResponse(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), { status, headers: { 'content-type': 'application/json' } });
}

// stubFetch 安装 fetch 路由：handler 返回 undefined 视为未打桩（抛错，测试立刻可见）。
export function stubFetch(
  handler: (url: string, init: RequestInit | undefined, calls: FetchCall[]) => Response | undefined,
): FetchCall[] {
  const calls: FetchCall[] = [];
  vi.stubGlobal('fetch', async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = String(input);
    calls.push({ url, init });
    const res = handler(url, init, calls);
    if (!res) throw new Error(`未打桩的请求: ${init?.method ?? 'GET'} ${url}`);
    return res;
  });
  return calls;
}

// countCalls 统计某路径的请求次数。
export function countCalls(calls: FetchCall[], path: string): number {
  return calls.filter((c) => c.url === path).length;
}

// rosterOf 构造名册视图。
export function rosterOf(names: string[], revision = 1): RosterView {
  return {
    services: names.map((name) => ({ name, addr: `ws://127.0.0.1:1/ws`, description: `${name} 测试服务` })),
    revision,
  };
}

// settingsOf 构造某服务的设置视图。
export function settingsOf(
  service: string,
  items: { key: string; value: string; default?: string; overridden?: boolean }[],
): ServiceSettingsView {
  return {
    service,
    settings: items.map((i) => ({
      key: i.key,
      value: i.value,
      default: i.default ?? '',
      overridden: i.overridden ?? false,
    })),
  };
}

// overviewOf 构造 overview 响应体。
export function overviewOf(r: RosterView, settingsList: ServiceSettingsView[] = []): Overview {
  return {
    connected: true,
    center: 'ws://127.0.0.1:9000/ws',
    self: { name: 'frontend', ws: 'ws://127.0.0.1:9501/ws', http: '127.0.0.1:9501' },
    roster: r,
    settings: { services: settingsList },
    fetchedAt: '2026-10-14T00:00:00Z',
  };
}
