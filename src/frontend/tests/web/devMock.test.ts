// devMock：开发模式才注入示例计时；真实计时存在时原样返回。
import assert from 'node:assert/strict';
import { describe, it } from 'vitest';
import { devTimers, mockTimers } from '../../src/web/devMock';
import type { TimerView } from '../../src/web/types';

describe('devMock', () => {
  it('示例计时是两条，都带 mock 标记与将来的到点时间', () => {
    const now = Date.parse('2026-10-30T12:00:00Z');
    const timers = mockTimers(now);
    assert.equal(timers.length, 2);
    for (const timer of timers) {
      assert.equal(timer.mock, true);
      assert.ok(new Date(timer.fireAt).getTime() > now);
    }
  });

  it('真实计时为空时补示例；已有真实计时就原样返回', () => {
    const injected = devTimers([]);
    assert.equal(injected.length, 2, '测试环境按开发模式处理');

    const real: TimerView[] = [
      { itemId: 'job-1', caller: 'reasoner', ms: 1000, fireAt: '2026-10-30T12:00:01Z', note: '' },
    ];
    assert.equal(devTimers(real), real);
  });
});
