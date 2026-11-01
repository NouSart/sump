// PortalView 的「重试」入口：上一轮推理失败但任务还开着时给一个明确的按钮，
// 而不是让用户自己猜怎么再触发一次。按钮点下去只调用一次 onRetry；没有失败时不渲染。
import assert from 'node:assert/strict';
import { describe, it } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { PortalView } from '../../src/web/components/PortalView';
import type { UploadResult } from '../../src/web/api';
import type { Translate } from '../../src/web/i18n';

const t: Translate = (key, fallback) => fallback ?? key;

function mount(over: {
  retryable?: boolean;
  onRetry?: () => Promise<void>;
  onSend?: (text: string, attachments: UploadResult[]) => Promise<boolean>;
  onUpload?: (file: File) => Promise<UploadResult>;
}) {
  return render(
    <PortalView
      connected
      messages={[]}
      timers={[]}
      approvals={[]}
      onDecide={async () => undefined}
      error={null}
      onSend={over.onSend ?? (async () => true)}
      onUpload={
        over.onUpload ??
        (async (file) => ({ kind: 'file', id: 'f-test', filename: file.name, mime: file.type, size: file.size }))
      }
      onTerminal={async () => ({ itemId: 'sh-test' })}
      terminalRuns={[]}
      onOpenSettings={() => undefined}
      tasks={[]}
      workflowExpand="focus"
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

describe('PortalView 附件', () => {
  it('选中文件后出现附件 chip，可移除', async () => {
    const { container } = mount({});
    const input = container.querySelector('input[type="file"]');
    assert.ok(input !== null);
    fireEvent.change(input, { target: { files: [new File(['hi'], '报告.txt', { type: 'text/plain' })] } });
    await waitFor(() => {
      assert.ok(screen.getByText('报告.txt'));
    });
    fireEvent.click(screen.getByRole('button', { name: /移除附件/ }));
    await waitFor(() => {
      assert.equal(screen.queryByText('报告.txt'), null);
    });
  });

  it('只有附件也能发：onSend 收到空文字 + 附件清单', async () => {
    const sent: { text: string; attachments: UploadResult[] }[] = [];
    const { container } = mount({
      onSend: async (text, attachments) => {
        sent.push({ text, attachments });
        return true;
      },
    });
    const input = container.querySelector('input[type="file"]');
    assert.ok(input !== null);
    fireEvent.change(input, { target: { files: [new File(['hi'], 'a.txt', { type: 'text/plain' })] } });
    await waitFor(() => {
      assert.ok(screen.getByText('a.txt'));
    });
    fireEvent.click(screen.getByRole('button', { name: '发送' }));
    await waitFor(() => {
      assert.equal(sent.length, 1);
    });
    assert.equal(sent[0]?.text, '');
    assert.equal(sent[0]?.attachments.length, 1);
    assert.equal(sent[0]?.attachments[0]?.filename, 'a.txt');
    // 发出去之后附件条清空（不然下一次会重复带同一个文件）。
    await waitFor(() => {
      assert.equal(screen.queryByText('a.txt'), null);
    });
  });

  it('上传失败时就地显示原因，不吞掉输入', async () => {
    const { container } = mount({
      onUpload: async () => {
        throw new Error('超过门户上限 20.0 MB');
      },
    });
    const input = container.querySelector('input[type="file"]');
    assert.ok(input !== null);
    fireEvent.change(input, { target: { files: [new File(['hi'], 'big.bin', { type: 'application/octet-stream' })] } });
    await waitFor(() => {
      assert.ok(screen.getByText(/big\.bin: 超过门户上限/));
    });
  });
});
