// 开发模式的示例数据：只在 `npm run dev` 下、且真实计时为空时注入两条计时消息，
// 好让消息卡片在没有真实计时的时候也能看得到。生产构建不会注入。
import type { TimerView } from './types';

/** mockTimers 造两条示例计时（90 秒后到点 / 10 分钟后到点）。 */
export function mockTimers(now = Date.now()): TimerView[] {
  return [
    {
      itemId: 'mock-timer-soon',
      caller: 'mock',
      ms: 90_000,
      fireAt: new Date(now + 90_000).toISOString(),
      note: '示例计时：90 秒',
      mock: true,
    },
    {
      itemId: 'mock-timer-long',
      caller: 'mock',
      ms: 600_000,
      fireAt: new Date(now + 600_000).toISOString(),
      note: '示例计时：10 分钟',
      mock: true,
    },
  ];
}

/** devTimers 在开发模式下给空列表补示例；已有真实计时就原样返回。 */
export function devTimers(existing: TimerView[]): TimerView[] {
  if (!import.meta.env.DEV || existing.length > 0) return existing;
  return mockTimers();
}
