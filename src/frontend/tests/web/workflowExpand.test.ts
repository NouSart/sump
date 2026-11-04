import assert from 'node:assert/strict';
import { act, renderHook } from '@testing-library/react';
import { beforeEach, describe, it } from 'vitest';
import {
  DefaultWorkflowExpand,
  useTraceOpen,
  useWorkflowExpand,
  type WorkflowExpand,
} from '../../src/web/workflowExpand';

const PrefKey = 'sump.portal.workflowExpand';

beforeEach(() => {
  window.localStorage.clear();
});

describe('useWorkflowExpand', () => {
  it('没有存过偏好时用默认档（焦点展开）', () => {
    const { result } = renderHook(() => useWorkflowExpand());
    assert.equal(result.current.mode, DefaultWorkflowExpand);
    assert.equal(result.current.mode, 'focus');
  });

  it('选了档位就记住，重新挂载还能读回来', () => {
    const first = renderHook(() => useWorkflowExpand());
    act(() => {
      first.result.current.setMode('expanded');
    });
    assert.equal(window.localStorage.getItem(PrefKey), 'expanded');

    const second = renderHook(() => useWorkflowExpand());
    assert.equal(second.result.current.mode, 'expanded');
  });

  it('localStorage 里是坏值/老值时退回默认档，不抛错', () => {
    window.localStorage.setItem(PrefKey, 'no-such-mode');
    const { result } = renderHook(() => useWorkflowExpand());
    assert.equal(result.current.mode, DefaultWorkflowExpand);
  });

  it('setMode 收到非法值也退回默认档', () => {
    const { result } = renderHook(() => useWorkflowExpand());
    act(() => {
      result.current.setMode('bogus' as WorkflowExpand);
    });
    assert.equal(result.current.mode, DefaultWorkflowExpand);
  });
});

// 造一个假的 <details>：useTraceOpen 只读 currentTarget.open 与 contains。
function fakeDetails(open: boolean) {
  const el = { open } as HTMLDetailsElement;
  const asEvent = <T,>(target: T): { currentTarget: T } => ({ currentTarget: target });
  return { el, asEvent };
}

describe('useTraceOpen', () => {
  it('折叠档：默认收起，用户点开就跟着开', () => {
    const { result } = renderHook(() => useTraceOpen('collapsed'));
    assert.equal(result.current.open, false);

    const { el, asEvent } = fakeDetails(true);
    act(() => {
      result.current.onToggle(asEvent(el) as never);
    });
    assert.equal(result.current.open, true);
  });

  it('展开档：默认展开，用户点收就跟着收', () => {
    const { result } = renderHook(() => useTraceOpen('expanded'));
    assert.equal(result.current.open, true);

    const { el, asEvent } = fakeDetails(false);
    act(() => {
      result.current.onToggle(asEvent(el) as never);
    });
    assert.equal(result.current.open, false);
  });

  it('焦点展开档：悬停才展开，移开就收起', () => {
    const { result } = renderHook(() => useTraceOpen('focus'));
    assert.equal(result.current.open, false);

    act(() => {
      result.current.handlers.onMouseEnter();
    });
    assert.equal(result.current.open, true);

    act(() => {
      result.current.handlers.onMouseLeave();
    });
    assert.equal(result.current.open, false);
  });

  it('焦点展开档：用户亲手点开之后，鼠标移开也不再抢着关', () => {
    const { result } = renderHook(() => useTraceOpen('focus'));
    act(() => {
      result.current.handlers.onMouseEnter();
    });
    assert.equal(result.current.open, true);

    // 用户在展开状态下点了一下 summary → 浏览器把 open 改成 false
    const { el, asEvent } = fakeDetails(false);
    act(() => {
      result.current.onToggle(asEvent(el) as never);
    });
    assert.equal(result.current.open, false);

    // 移开鼠标：仍然是用户要的收起状态
    act(() => {
      result.current.handlers.onMouseLeave();
    });
    assert.equal(result.current.open, false);

    // 移回去：用户已表态优先，不会被自动展开
    act(() => {
      result.current.handlers.onMouseEnter();
    });
    assert.equal(result.current.open, false);
  });

  it('切档会清掉用户的手动意志，按新档位重新算', () => {
    const { result, rerender } = renderHook(({ mode }: { mode: WorkflowExpand }) => useTraceOpen(mode), {
      initialProps: { mode: 'expanded' as WorkflowExpand },
    });
    assert.equal(result.current.open, true);

    const { el, asEvent } = fakeDetails(false);
    act(() => {
      result.current.onToggle(asEvent(el) as never);
    });
    assert.equal(result.current.open, false);

    // 换成"展开"档（已在这一档，换到折叠再换回来）
    rerender({ mode: 'collapsed' });
    assert.equal(result.current.open, false);
    rerender({ mode: 'expanded' });
    assert.equal(result.current.open, true);
  });

  it('程序自己改 open 不触发 onToggle 时不会被误判成用户操作', () => {
    const { result } = renderHook(() => useTraceOpen('expanded'));
    // 浏览器状态与算出来的 open 一致 → 不算用户动的手
    const { el, asEvent } = fakeDetails(true);
    act(() => {
      result.current.onToggle(asEvent(el) as never);
    });
    assert.equal(result.current.open, true);
  });
});
