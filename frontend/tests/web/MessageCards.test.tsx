// 消息卡片：只收终端消息与计时消息、新的在最后、分类计数、收纳气泡与短暂展示、
// 逐张放出的节奏、悬停加宽/终端窗口、暂停→转圈→继续、停止。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, fireEvent, render, screen } from '@testing-library/react';
import { MessageCards } from '../../src/web/components/MessageCards';
import type { FeedCard } from '../../src/web/messageFeed';
import type { Translate } from '../../src/web/i18n';

const tZh: Translate = (key, fallback) => fallback ?? key;

const now = Date.parse('2026-10-30T12:00:00Z');

function terminalCard(id: string, command: string, at: string): FeedCard {
  return {
    kind: 'terminal',
    id: `terminal:${id}`,
    at,
    title: 'shell__run',
    preview: command,
    taskId: 't1',
    taskName: '任务一',
    tool: 'shell__run',
    command,
  };
}

function timerCard(id: string, note: string, at: string, fireInMS = 90_000): FeedCard {
  // 进度条的分子分母都按 at → fireAt 计算，所以 at 默认取"现在"，避免测试里进度直接满格。
  const start = Date.parse(at);
  const from = Number.isFinite(start) && Math.abs(start - Date.now()) < 6 * 3600_000 ? start : Date.now();
  return {
    kind: 'timer',
    id: `timer:${id}`,
    at: new Date(from).toISOString(),
    title: note,
    note,
    caller: 'reasoner',
    fireAt: new Date(from + fireInMS).toISOString(),
    ms: fireInMS,
  };
}

function renderCards(cards: FeedCard[], peekSeconds = 4) {
  return render(<MessageCards cards={cards} peekSeconds={peekSeconds} t={tZh} />);
}

describe('MessageCards', () => {
  it('没有卡片时什么都不渲染', () => {
    const { container } = renderCards([]);
    assert.equal(container.querySelector('.mcards'), null);
  });

  it('卡片按时间排列：旧的在前、新的在最后（从下往上长）', () => {
    vi.useFakeTimers();
    try {
      const { container } = renderCards([
        terminalCard('1', '第一条命令', '2026-10-30T11:00:00Z'),
        timerCard('2', '第二条计时', '2026-10-30T11:10:00Z'),
      ]);
      act(() => {
        vi.advanceTimersByTime(200);
      });
      const titles = Array.from(container.querySelectorAll('.mcard-title')).map((e) => e.textContent);
      assert.deepEqual(titles, ['shell__run', '第二条计时']);
    } finally {
      vi.useRealTimers();
    }
  });

  it('新消息排在最后：旧卡片不消失，被向上挤压', () => {
    vi.useFakeTimers();
    try {
      const first = terminalCard('1', '第一条', '2026-10-30T11:00:00Z');
      const second = terminalCard('2', '第二条', '2026-10-30T11:01:00Z');
      const { container, rerender } = render(<MessageCards cards={[first]} peekSeconds={4} t={tZh} />);
      assert.equal(container.querySelectorAll('.mcard').length, 1);

      rerender(<MessageCards cards={[first, second]} peekSeconds={4} t={tZh} />);
      assert.equal(container.querySelectorAll('.mcard').length, 1, '新卡片仍在排队');
      act(() => {
        vi.advanceTimersByTime(200);
      });
      assert.equal(container.querySelectorAll('.mcard').length, 2, '两张都在，旧的没被挤掉');
      const bodies = Array.from(container.querySelectorAll('.mcard-body')).map((e) => e.textContent);
      assert.deepEqual(bodies, ['第一条', '第二条'], '新的在最后（最下面）');
    } finally {
      vi.useRealTimers();
    }
  });

  it('新消息到来时自动跟到最新（贴底状态不会被新增内容破坏）', () => {
    vi.useFakeTimers();
    try {
      const first = terminalCard('1', '第一条', '2026-10-30T11:00:00Z');
      const second = terminalCard('2', '第二条', '2026-10-30T11:01:00Z');
      const { container, rerender } = render(<MessageCards cards={[first]} peekSeconds={4} t={tZh} />);
      const list = container.querySelector('.mcards-list') as HTMLElement;
      Object.defineProperty(list, 'scrollHeight', { configurable: true, value: 900 });

      rerender(<MessageCards cards={[first, second]} peekSeconds={4} t={tZh} />);
      act(() => {
        vi.advanceTimersByTime(200);
      });
      assert.equal(list.scrollTop, 900, '应自动滚到最新一张');
    } finally {
      vi.useRealTimers();
    }
  });

  it('多条同时到达：每 200ms 放出一张', () => {
    vi.useFakeTimers();
    try {
      const { container } = renderCards([
        terminalCard('1', '一', '2026-10-30T11:00:00Z'),
        terminalCard('2', '二', '2026-10-30T11:01:00Z'),
        terminalCard('3', '三', '2026-10-30T11:02:00Z'),
      ]);
      assert.equal(container.querySelectorAll('.mcard').length, 1, '首张立即出现');
      act(() => {
        vi.advanceTimersByTime(200);
      });
      assert.equal(container.querySelectorAll('.mcard').length, 2);
      act(() => {
        vi.advanceTimersByTime(200);
      });
      assert.equal(container.querySelectorAll('.mcard').length, 3);
    } finally {
      vi.useRealTimers();
    }
  });

  it('分类计数：计时与终端各算各的（抬头已去掉，计数体现在气泡里）', () => {
    renderCards([
      terminalCard('1', '一', '2026-10-30T11:00:00Z'),
      terminalCard('2', '二', '2026-10-30T11:01:00Z'),
      timerCard('3', '计时', '2026-10-30T11:02:00Z'),
    ]);
    fireEvent.click(screen.getByRole('button', { name: '收纳消息' }));
    const label = screen.getByRole('button', { name: /展开消息/ }).getAttribute('aria-label') ?? '';
    assert.match(label, /计时 1/);
    assert.match(label, /终端 2/);
  });

  it('点击展开、移出收回：普通卡片加宽；终端消息变成消息模式终端窗口', () => {
    vi.useFakeTimers();
    try {
      const { container } = renderCards([
        terminalCard('1', 'Get-Date', '2026-10-30T11:00:00Z'),
        timerCard('2', '等构建', ''),
      ]);
      act(() => {
        vi.advanceTimersByTime(200);
      });
      const [terminal, timer] = Array.from(container.querySelectorAll('.mcard')) as HTMLElement[];

      assert.equal(timer!.style.width, '320px', '没点之前保持基准宽度');
      fireEvent.click(timer!);
      assert.equal(timer!.style.width, '344px', '点一下展开：未省略 → 略加宽 24px');
      fireEvent.mouseLeave(timer!);
      assert.equal(timer!.style.width, '320px', '移出收回');

      fireEvent.click(terminal!);
      assert.ok(terminal!.classList.contains('is-terminal'));
      assert.equal(terminal!.style.width, '480px');
    } finally {
      vi.useRealTimers();
    }
  });

  it('收起入口：右下角按钮、Esc、双击卡片区都能收纳', () => {
    const { container } = renderCards([terminalCard('1', '一', '2026-10-30T11:00:00Z')]);
    const expanded = (): boolean => screen.queryByRole('button', { name: '收纳消息' }) !== null;

    fireEvent.click(screen.getByRole('button', { name: '收纳消息' }));
    assert.equal(expanded(), false, '右下角按钮可收起');

    fireEvent.click(screen.getByRole('button', { name: /展开消息/ }));
    assert.equal(expanded(), true);

    fireEvent.keyDown(window, { key: 'Escape' });
    assert.equal(expanded(), false, 'Esc 可收起');

    fireEvent.click(screen.getByRole('button', { name: /展开消息/ }));
    fireEvent.doubleClick(container.querySelector('.mcards-list') as HTMLElement);
    assert.equal(expanded(), false, '双击卡片区可收起');
  });

  it('收纳成气泡：只用图标显示有数量的类别，展开再点收起', () => {
    renderCards([terminalCard('1', '一', '2026-10-30T11:00:00Z'), timerCard('2', '计时', '2026-10-30T11:01:00Z')]);

    fireEvent.click(screen.getByRole('button', { name: '收纳消息' }));
    const bubble = screen.getByRole('button', { name: /展开消息/ });
    // 气泡里不出现中文分类名，只有图标 + 数字；读屏文案仍带分类。
    assert.equal(bubble.textContent?.includes('计时'), false);
    assert.equal(bubble.textContent?.includes('终端'), false);
    assert.match(bubble.getAttribute('aria-label') ?? '', /计时 1/);
    assert.match(bubble.getAttribute('aria-label') ?? '', /终端 1/);
    assert.equal(bubble.className.includes('bubble'), true, '复用左栏气泡的材质类');

    fireEvent.click(bubble);
    assert.ok(screen.getByRole('button', { name: '收纳消息' }), '点气泡恢复展开');
  });

  it('气泡只显示有数量的类别：没有计时就不出现计时', () => {
    renderCards([terminalCard('1', '一', '2026-10-30T11:00:00Z')]);
    fireEvent.click(screen.getByRole('button', { name: '收纳消息' }));
    const label = screen.getByRole('button', { name: /展开消息/ }).getAttribute('aria-label') ?? '';
    assert.match(label, /终端 1/);
    assert.equal(label.includes('计时'), false);
  });

  it('示例计时带 mock 标记', () => {
    const card = timerCard('mock-1', '示例计时', '2026-10-30T11:00:00Z');
    renderCards([{ ...card, mock: true }]);
    assert.ok(screen.getByText('mock'));
  });

  it('收起后新消息只滑气泡 N 秒，不强行撑开', () => {
    vi.useFakeTimers();
    try {
      const card1 = terminalCard('1', '一', '2026-10-30T11:00:00Z');
      const { rerender } = render(<MessageCards cards={[card1]} peekSeconds={4} t={tZh} />);
      fireEvent.click(screen.getByRole('button', { name: '收纳消息' }));
      assert.equal(screen.queryByRole('button', { name: '收纳消息' }), null, '已收起');

      const card2 = terminalCard('2', '新来的', '2026-10-30T11:05:00Z');
      rerender(<MessageCards cards={[card1, card2]} peekSeconds={4} t={tZh} />);

      const bubble = screen.getByRole('button', { name: /展开消息/ });
      assert.ok(bubble.classList.contains('is-peek'), '新消息到达时气泡短暂展示');
      assert.ok(bubble.textContent?.includes('新来的'));
      assert.equal(screen.queryByRole('button', { name: '收纳消息' }), null, '不强行撑开');

      act(() => {
        vi.advanceTimersByTime(4000);
      });
      assert.equal(screen.getByRole('button', { name: /展开消息/ }).classList.contains('is-peek'), false);
    } finally {
      vi.useRealTimers();
    }
  });

  it('计时卡片：进度条 + 每秒更新的倒计时；暂停后冻结', () => {
    vi.useFakeTimers();
    try {
      renderCards([timerCard('1', '等构建', '')]);
      const bar = screen.getByRole('progressbar');
      const before = Number(bar.getAttribute('aria-valuenow'));
      assert.ok(before >= 0 && before < 100, `进度应在 0..100 之间，实际 ${String(before)}`);
      assert.match(screen.getByText(/后唤醒/).textContent ?? '', /^\d{2}:\d{2}/);

      act(() => {
        vi.advanceTimersByTime(3000);
      });
      const later = Number(screen.getByRole('progressbar').getAttribute('aria-valuenow'));
      assert.ok(later > before, '倒计时在走，进度条要前进');

      // 暂停：等待转圈结束 → 之后读数与进度都冻结
      fireEvent.click(screen.getByRole('button', { name: '暂停' }));
      act(() => {
        vi.advanceTimersByTime(900);
      });
      const frozen = screen.getByRole('progressbar').getAttribute('aria-valuenow');
      act(() => {
        vi.advanceTimersByTime(3000);
      });
      assert.equal(screen.getByRole('progressbar').getAttribute('aria-valuenow'), frozen, '暂停期间进度冻结');
      assert.ok(screen.getByRole('button', { name: '继续' }));
    } finally {
      vi.useRealTimers();
    }
  });

  it('到点后显示「到点…」，4 秒后淡出移除', () => {
    vi.useFakeTimers();
    try {
      const { container } = renderCards([timerCard('1', '马上到点', '', 1500)]);
      act(() => {
        vi.advanceTimersByTime(2000);
      });
      assert.ok(screen.getByText('到点…'));
      assert.ok((container.querySelector('.mcard') as HTMLElement).classList.contains('is-expired'));

      act(() => {
        vi.advanceTimersByTime(4000);
      });
      assert.equal(container.querySelector('.mcard'), null, '4 秒后移除');
    } finally {
      vi.useRealTimers();
    }
  });

  it('暂停 → 转圈 → 继续；继续同理；停止后按钮消失', () => {
    vi.useFakeTimers();
    try {
      renderCards([timerCard('1', '等构建', '')]);

      fireEvent.click(screen.getByRole('button', { name: '暂停' }));
      const spinning = screen.getByRole('button', { name: '暂停' });
      assert.equal(spinning.getAttribute('aria-busy'), 'true');
      assert.ok(spinning.querySelector('.mcard-spinner'));

      act(() => {
        vi.advanceTimersByTime(900);
      });
      fireEvent.click(screen.getByRole('button', { name: '继续' }));
      act(() => {
        vi.advanceTimersByTime(900);
      });
      assert.ok(screen.getByRole('button', { name: '暂停' }), '继续之后回到暂停');

      fireEvent.click(screen.getByRole('button', { name: '停止' }));
      assert.ok(screen.getByText('已停止'));
      expect(screen.queryByRole('button', { name: '暂停' })).toBeNull();
    } finally {
      vi.useRealTimers();
    }
  });
});
