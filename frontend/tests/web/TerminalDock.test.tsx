// 终端：放大的输入框，但命令是真的执行——这里只验证"发出去 → 回显 → 补报到达后画出来"。
// 审批归 approval 服务，终端不做裁决：rejected 就显示原因，approved 就说明开始执行。
import assert from 'node:assert/strict';
import { describe, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { TerminalDock } from '../../src/web/components/TerminalDock';
import type { TerminalRunUpdate } from '../../src/web/types';
import type { Translate } from '../../src/web/i18n';

const tZh: Translate = (key, fallback) => fallback ?? key;

function update(over: Partial<TerminalRunUpdate>): TerminalRunUpdate {
  return {
    itemId: 'sh-1',
    phase: 'done',
    command: 'echo hi',
    text: '',
    exitCode: 0,
    stdout: '',
    stderr: '',
    error: '',
    truncated: false,
    timedOut: false,
    durationMs: 1,
    final: true,
    at: '2026-11-01T00:00:00.000Z',
    ...over,
  };
}

describe('TerminalDock', () => {
  it('关闭时不渲染', () => {
    const { container } = render(
      <TerminalDock open={false} onClose={() => {}} onRun={async () => ({ itemId: 'x' })} updates={[]} t={tZh} />,
    );
    assert.equal(container.querySelector('.terminal-pane'), null);
  });

  it('打开后显示空态与"经审批执行"的说明', () => {
    render(<TerminalDock open onClose={() => {}} onRun={async () => ({ itemId: 'x' })} updates={[]} t={tZh} />);
    assert.ok(screen.getByRole('region', { name: '终端' }));
    assert.ok(screen.getByText(/终端还没有输出/));
    assert.ok(screen.getByText('命令经审批后执行 · 不是交互终端'));
  });

  it('回车把命令交给 onRun，并回显等待态', async () => {
    const ran: string[] = [];
    render(
      <TerminalDock
        open
        onClose={() => {}}
        onRun={async (cmd) => {
          ran.push(cmd);
          return { itemId: 'sh-9' };
        }}
        updates={[]}
        t={tZh}
      />,
    );
    const input = screen.getByLabelText('终端命令') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'ls -la' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => {
      assert.deepEqual(ran, ['ls -la']);
    });
    assert.ok(screen.getByText('ls -la'));
    assert.ok(screen.getByText('已提交，等待审批或执行…'));
    assert.equal(input.value, '');
  });

  it('提交失败：错误就地显示，命令留在输入框里', async () => {
    render(
      <TerminalDock
        open
        onClose={() => {}}
        onRun={async () => {
          throw new Error('未知目标服务: shell（不在名册里）');
        }}
        updates={[]}
        t={tZh}
      />,
    );
    const input = screen.getByLabelText('终端命令') as HTMLInputElement;
    fireEvent.change(input, { target: { value: 'echo hi' } });
    fireEvent.keyDown(input, { key: 'Enter' });
    await waitFor(() => {
      assert.ok(screen.getByRole('alert'));
    });
    assert.match(screen.getByRole('alert').textContent ?? '', /未知目标服务/);
    assert.equal(input.value, 'echo hi');
  });

  it('补报到达：审批通过 → 输出 → 退出码', () => {
    const updates = [
      update({ phase: 'approved', final: false, at: '2026-11-01T00:00:01.000Z' }),
      update({ phase: 'done', exitCode: 0, stdout: 'hi\n', at: '2026-11-01T00:00:02.000Z' }),
    ];
    render(<TerminalDock open onClose={() => {}} onRun={async () => ({ itemId: 'sh-1' })} updates={updates} t={tZh} />);
    assert.ok(screen.getByText('审批通过，开始执行'));
    assert.ok(screen.getByText(/hi/));
    assert.ok(screen.getByText('exit 0'));
  });

  it('被拒：显示原因，不显示退出码', () => {
    const updates = [update({ phase: 'rejected', final: true, error: '审批未通过：用户拒绝' })];
    render(<TerminalDock open onClose={() => {}} onRun={async () => ({ itemId: 'sh-1' })} updates={updates} t={tZh} />);
    assert.ok(screen.getByText(/用户拒绝/));
    assert.equal(screen.queryByText(/^exit /), null);
  });

  it('截断与超时都在退出码行里说明', () => {
    const updates = [update({ phase: 'done', exitCode: 0, stdout: 'x', truncated: true, timedOut: true })];
    render(<TerminalDock open onClose={() => {}} onRun={async () => ({ itemId: 'sh-1' })} updates={updates} t={tZh} />);
    assert.ok(screen.getByText('exit 0（超时终止，输出已截断）'));
  });

  it('同一条补报重复到达时只画一次', () => {
    const one = update({ phase: 'done', stdout: 'once' });
    const { rerender } = render(
      <TerminalDock open onClose={() => {}} onRun={async () => ({ itemId: 'sh-1' })} updates={[one]} t={tZh} />,
    );
    rerender(<TerminalDock open onClose={() => {}} onRun={async () => ({ itemId: 'sh-1' })} updates={[one]} t={tZh} />);
    assert.equal(screen.getAllByText(/once/).length, 1);
  });

  it('Esc 与关闭按钮都触发 onClose', () => {
    const onClose = vi.fn();
    render(<TerminalDock open onClose={onClose} onRun={async () => ({ itemId: 'x' })} updates={[]} t={tZh} />);
    fireEvent.keyDown(window, { key: 'Escape' });
    assert.equal(onClose.mock.calls.length, 1);

    fireEvent.click(screen.getByRole('button', { name: '关闭终端' }));
    assert.equal(onClose.mock.calls.length, 2);
  });
});
