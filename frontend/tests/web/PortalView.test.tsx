// PortalView 的「重试」入口：上一轮推理失败但任务还开着时给一个明确的按钮，
// 而不是让用户自己猜怎么再触发一次。按钮点下去只调用一次 onRetry；没有失败时不渲染。
import assert from 'node:assert/strict';
import { describe, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { PortalView } from '../../src/web/components/PortalView';
import type { Translate } from '../../src/web/i18n';

const t: Translate = (key, fallback) => fallback ?? key;

function mount(over: { retryable?: boolean; onRetry?: () => Promise<void> }) {
  return render(
    <PortalView
      connected
      messages={[]}
      timers={[]}
      approvals={[]}
      onDecide={async () => undefined}
      error={null}
      onSend={async () => true}
      onOpenSettings={() => undefined}
      tasks={[]}
      workflowExpand="auto"
      retryable={over.retryable}
      onRetry={over.onRetry}
      t={t}
    />,
  );
}

describe('PortalView 重试入口', () => {
  it('没有失败时不渲染重试按钮', () => {
    mount({ retryable: false, onRetry: async () => undefined });
    assert.equal(screen.queryByText('重试'), null);
  });

  it('失败时显示重试按钮并说明任务还在', () => {
    mount({ retryable: true, onRetry: async () => undefined });
    assert.ok(screen.getByText('上一轮推理失败，任务还在'));
    assert.ok(screen.getByRole('button', { name: '重试' }));
  });

  it('点一次只调用一次 onRetry', async () => {
    let calls = 0;
    mount({
      retryable: true,
      onRetry: async () => {
        calls += 1;
      },
    });
    fireEvent.click(screen.getByRole('button', { name: '重试' }));
    await waitFor(() => {
      assert.equal(calls, 1);
    });
  });
});
