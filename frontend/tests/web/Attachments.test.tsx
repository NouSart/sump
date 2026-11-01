// 别的入口带来的附件在门户里要能看见。
//
// QQ 的图片 / 文件在记录里只有 id（meta.images / meta.files）：门户按 id 惰性取，
// 不把 base64 塞进历史。这里守两件事：图片内联成 <img>（src 指向 /api/attachment），
// 文件点一下才去取文本预览——不看就不花那次跨服务调用。
import assert from 'node:assert/strict';
import { afterEach, describe, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { PortalView } from '../../src/web/components/PortalView';
import { parseAttachments } from '../../src/web/api';
import type { PortalMessage } from '../../src/web/types';
import type { Translate } from '../../src/web/i18n';

const t: Translate = (key, fallback) => fallback ?? key;

function renderWith(messages: PortalMessage[]) {
  return render(
    <PortalView
      connected
      messages={messages}
      timers={[]}
      approvals={[]}
      onDecide={async () => undefined}
      error={null}
      onSend={async () => true}
      onUpload={async (file) => ({
        kind: 'file',
        id: 'f-test',
        filename: file.name,
        mime: file.type,
        size: file.size,
      })}
      onTerminal={async () => ({ itemId: 'sh-test' })}
      terminalRuns={[]}
      onOpenSettings={() => undefined}
      tasks={[]}
      workflowExpand="focus"
      t={t}
    />,
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('parseAttachments', () => {
  it('取出 images / files 的 id，脏数据丢掉', () => {
    const raw = JSON.stringify({
      sender: { channel: 'group' },
      images: ['img-1', '../../etc/passwd', 42],
      files: ['file-1'],
    });
    assert.deepEqual(parseAttachments(raw), [
      { kind: 'image', id: 'img-1' },
      { kind: 'file', id: 'file-1' },
    ]);
    assert.equal(parseAttachments(undefined), undefined);
    assert.equal(parseAttachments(''), undefined);
    assert.equal(parseAttachments('不是 JSON'), undefined);
    assert.equal(parseAttachments(JSON.stringify({ images: [] })), undefined);
  });
});

describe('门户里的附件', () => {
  it('图片内联成 <img>，src 指向按 id 取的接口', () => {
    const { container } = renderWith([
      {
        author: 'user',
        kind: 'user',
        text: '看看这张图',
        at: '2026-11-01T10:00:00Z',
        attachments: [{ kind: 'image', id: 'img-1' }],
      },
    ]);
    const img = container.querySelector('img.step-attach-img');
    assert.ok(img !== null, '应渲染图片附件');
    assert.equal(img.getAttribute('src'), '/api/attachment?kind=image&id=img-1');
  });

  it('文件附件点开才去取预览，取到后显示文件名与正文', async () => {
    const fetchMock = vi.fn(async () =>
      new Response(JSON.stringify({ filename: '报告.md', text: '正文内容', next: 0, total: 4 }), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      }),
    );
    vi.stubGlobal('fetch', fetchMock);

    renderWith([
      {
        author: 'user',
        kind: 'user',
        text: '附件给你',
        at: '2026-11-01T10:00:00Z',
        attachments: [{ kind: 'file', id: 'file-1' }],
      },
    ]);
    // 打开之前不取（惰性）。
    assert.equal(fetchMock.mock.calls.length, 0);

    fireEvent.click(screen.getByRole('button', { name: /文件|查看/ }));
    await waitFor(() => {
      assert.ok(screen.getByText('正文内容'));
    });
    assert.equal(fetchMock.mock.calls.length, 1);
    assert.equal((fetchMock.mock.calls[0] as unknown[])[0], '/api/attachment?kind=file&id=file-1');
    assert.ok(screen.getByText('报告.md'));
  });

  it('取不到时如实显示原因（不假装没有附件）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () => new Response(JSON.stringify({ error: '文件是二进制的，读不了' }), { status: 502 })),
    );
    renderWith([
      {
        author: 'user',
        kind: 'user',
        text: '附件',
        at: '2026-11-01T10:00:00Z',
        attachments: [{ kind: 'file', id: 'file-2' }],
      },
    ]);
    fireEvent.click(screen.getByRole('button', { name: /文件|查看/ }));
    await waitFor(() => {
      assert.ok(screen.getByText(/二进制/));
    });
  });
});
