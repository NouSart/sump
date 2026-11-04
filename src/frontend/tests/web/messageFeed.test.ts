// messageFeed：只收终端消息与计时消息、按时间正序、分类计数。
import assert from 'node:assert/strict';
import { describe, it } from 'vitest';
import { buildFeed, feedCounts, hasLineBreak } from '../../src/web/messageFeed';
import type { PortalMessage, TimerView } from '../../src/web/types';

function msg(over: Partial<PortalMessage>): PortalMessage {
  return { author: 'agent', text: '', at: '2026-10-30T11:00:00Z', ...over };
}

describe('messageFeed', () => {
  it('只收 tool_call；思考、输出、答复都不进消息栏', () => {
    const cards = buildFeed({
      messages: [
        msg({ kind: 'reasoning', text: '想一想', seq: 1 }),
        msg({ kind: 'tool_call', text: 'Get-Date', seq: 2, meta: { tool: 'shell__run' } }),
        msg({ kind: 'tool_result', text: '输出内容', seq: 3 }),
        msg({ kind: 'assistant', text: '答复', seq: 4 }),
      ],
      timers: [],
      tasks: [],
    });
    assert.equal(cards.length, 1);
    assert.equal(cards[0]?.kind, 'terminal');
    assert.equal(cards[0]?.kind === 'terminal' ? cards[0].command : '', 'Get-Date');
  });

  it('计时消息来自 timer 快照，时间取开始时刻', () => {
    const timer: TimerView = {
      itemId: 'job-1',
      caller: 'reasoner',
      ms: 60_000,
      fireAt: '2026-10-30T11:01:00Z',
      note: '等构建',
    };
    const cards = buildFeed({ messages: [], timers: [timer], tasks: [] });
    assert.equal(cards.length, 1);
    assert.equal(cards[0]?.kind, 'timer');
    assert.equal(cards[0]?.at, '2026-10-30T11:00:00.000Z');
  });

  it('按时间正序：旧的在前、新的在后', () => {
    const cards = buildFeed({
      messages: [
        msg({ kind: 'tool_call', text: '晚', seq: 1, at: '2026-10-30T12:00:00Z' }),
        msg({ kind: 'tool_call', text: '早', seq: 2, at: '2026-10-30T10:00:00Z' }),
      ],
      timers: [],
      tasks: [],
    });
    assert.deepEqual(
      cards.map((card) => (card.kind === 'terminal' ? card.command : card.note)),
      ['早', '晚'],
    );
  });

  it('分类计数', () => {
    const cards = buildFeed({
      messages: [msg({ kind: 'tool_call', text: '一', seq: 1 })],
      timers: [
        { itemId: 'a', caller: '', ms: 1000, fireAt: '2026-10-30T11:00:01Z', note: '' },
        { itemId: 'b', caller: '', ms: 1000, fireAt: '2026-10-30T11:00:02Z', note: '' },
      ],
      tasks: [],
    });
    assert.deepEqual(feedCounts(cards), { terminal: 1, timer: 2 });
  });

  it('含换行的原文优先上下伸开', () => {
    assert.equal(hasLineBreak('a\nb'), true);
    assert.equal(hasLineBreak('a b'), false);
  });
});
