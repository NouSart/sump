// ApprovalStrip：待审命令必须完整可见；只有批准/拒绝两个动作；提交中两个按钮都禁用。
import assert from 'node:assert/strict';
import { describe, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { ApprovalStrip } from '../../src/web/components/ApprovalStrip';
import type { Translate } from '../../src/web/i18n';
import type { PendingApprovalView } from '../../src/web/types';

const t: Translate = (key, fallback) => fallback ?? key;

const item: PendingApprovalView = {
  itemId: 'item-7',
  command: 'Remove-Item -Recurse C:\\work\\build',
  workDir: 'C:\\work',
  requestedBy: 'shell',
  reasonCode: 'NO_MATCHING_RULE',
  summary: '没有覆盖该命令的允许规则',
  unknowns: [],
  ruleIds: [],
  createdMs: 0,
  expiresMs: 0,
};

describe('ApprovalStrip', () => {
  it('没有待审项时不渲染', () => {
    const { container } = render(<ApprovalStrip t={t} items={[]} onDecide={async () => {}} />);
    assert.equal(container.querySelector('.approvals'), null);
  });

  it('命令原文完整可见，批准与拒绝各回传一次', async () => {
    const decisions: [string, string][] = [];
    const { container } = render(
      <ApprovalStrip
        t={t}
        items={[item]}
        onDecide={async (itemId, decision) => {
          decisions.push([itemId, decision]);
        }}
      />,
    );

    // 不是“页面上有这段字”，而是待审区的 <pre> 原文一字不差。
    assert.equal(container.querySelector('.approval-cmd')?.textContent, item.command);
    assert.match(screen.getByText(/没有覆盖该命令/).textContent ?? '', /NO_MATCHING_RULE/);

    fireEvent.click(screen.getByLabelText('批准执行'));
    await waitFor(() => assert.deepEqual(decisions, [['item-7', 'allow']]));
    fireEvent.click(screen.getByLabelText('拒绝'));
    await waitFor(() => assert.deepEqual(decisions, [['item-7', 'allow'], ['item-7', 'deny']]));
  });

  it('提交中两个按钮都禁用，完成后恢复可点', async () => {
    let finish: () => void = () => {};
    const pending = new Promise<void>((resolve) => {
      finish = resolve;
    });
    render(<ApprovalStrip t={t} items={[item]} onDecide={() => pending} />);

    const approve = screen.getByLabelText('批准执行');
    const deny = screen.getByLabelText('拒绝');
    fireEvent.click(approve);
    await waitFor(() => assert.equal(approve.getAttribute('aria-busy'), 'true'));
    assert.equal((approve as HTMLButtonElement).disabled, true);
    assert.equal((deny as HTMLButtonElement).disabled, true);

    finish();
    await waitFor(() => assert.equal(approve.getAttribute('aria-busy'), 'false'));
    assert.equal((approve as HTMLButtonElement).disabled, false);
    assert.equal((deny as HTMLButtonElement).disabled, false);
  });
});
