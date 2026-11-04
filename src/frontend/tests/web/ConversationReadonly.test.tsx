// 只读会话（别的入口，比如 QQ）：门户里看得见它的记录，但不能在这里说话。
//
// 三件事必须成立，否则"在门户里看 QQ"要么白配、要么危险：
//   1) 侧栏列出席 QQ 会话，点开能看见它的对话；
//   2) 输入框、附件、终端全禁用，并明确告诉用户"回 QQ 里回复"；
//   3) 只读会话收不到 SSE 推送，所以要按 after_seq 增量轮询（否则新消息永远不出现）。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, waitFor } from '@testing-library/react';
import { App } from '../../src/web/App';
import { installFakeEventSource, jsonResponse, overviewOf, rosterOf, stubFetch } from './harness';

const PORTAL = {
  id: 'web:private:owner',
  title: '',
  entry: 'web',
  status: 'active' as const,
  created_at: '2026-11-01T09:00:00Z',
  updated_at: '2026-11-01T10:00:00Z',
  readonly: false,
};

const QQ_GROUP = {
  id: 'qq:group:67890',
  title: 'QQ 群「编程交流」',
  entry: 'qq',
  status: 'active' as const,
  created_at: '2026-11-01T08:00:00Z',
  updated_at: '2026-11-01T09:30:00Z',
  readonly: true,
};

// bootReadOnly 起一个"门户 + QQ 群"的环境：QQ 会话里有两条记录，带上来源标注。
async function bootReadOnly(): Promise<{ calls: { url: string }[] }> {
  const calls = stubFetch((url) => {
    if (url === '/api/overview') {
      return jsonResponse(overviewOf(rosterOf(['memory', 'qq'])));
    }
    if (url === '/api/conversations') {
      return jsonResponse({ conversations: [PORTAL, QQ_GROUP] });
    }
    if (url.startsWith('/api/tasks')) {
      const cid = url.includes('qq%3Agroup%3A67890') ? QQ_GROUP.id : PORTAL.id;
      return jsonResponse({
        conversationId: cid,
        tasks:
          cid === QQ_GROUP.id
            ? [{ id: 'qtask-1', conversation_id: cid, name: '群里问几点开会', status: 'active', created_at: '2026-11-01T09:00:00Z', updated_at: '2026-11-01T09:30:00Z' }]
            : [],
      });
    }
    if (url.startsWith('/api/history')) {
      if (url.includes('qq%3Agroup%3A67890')) {
        return jsonResponse({
          conversationId: QQ_GROUP.id,
          messages: [
            {
              seq: 11,
              role: 'user',
              kind: 'user',
              content: '[群聊 · 群「编程交流」(67890) · 12 人可见 · 张三(12345) · 非主人 · 未@我] 今天几点开会',
              at: '2026-11-01T09:29:00Z',
              task_id: 'qtask-1',
            },
            {
              seq: 12,
              role: 'assistant',
              kind: 'assistant',
              content: '三点',
              at: '2026-11-01T09:30:00Z',
              task_id: 'qtask-1',
            },
          ],
        });
      }
      return jsonResponse({ conversationId: PORTAL.id, messages: [] });
    }
    return undefined;
  });
  installFakeEventSource();
  render(<App />);
  // 侧栏里 QQ 群排在第二（按 updated_at 之外的给定顺序），点它切过去。
  fireEvent.click(await screen.findByRole('button', { name: /QQ 群「编程交流」/ }));
  return { calls };
}

describe('只读会话（QQ）', () => {
  it('点开 QQ 会话能看到它的记录，输入区变成只读', async () => {
    await bootReadOnly();

    // 记录真的显示出来了（带来源标注，能看出是谁在哪个群说的）。
    assert.ok(await screen.findByText(/今天几点开会/), 'QQ 群里的消息应显示在面板上');
    assert.ok(screen.getByText('三点'), '智能体在群里的答复也应显示');

    // 输入区：禁用 + 一句解释；附件 / 发送 / 终端都不可点。
    const input = screen.getByLabelText('消息') as HTMLTextAreaElement;
    assert.equal(input.disabled, true);
    assert.equal(input.getAttribute('placeholder'), '只读查看（回复请去 QQ）');
    assert.ok(screen.getByText(/只读查看：这是 QQ 里的对话/), '应说明回复要去 QQ');
    for (const name of ['发送', '终端']) {
      assert.equal((screen.getByLabelText(name) as HTMLButtonElement).disabled, true, `${name} 应禁用`);
    }
    // 附件按钮与隐藏的 file input 同名，取按钮那一个。
    const attach = screen.getAllByLabelText('添加附件').find((el) => el.tagName === 'BUTTON') as HTMLButtonElement;
    assert.equal(attach.disabled, true, '添加附件 应禁用');
  });

  it('QQ 会话没有改名 / 归档按钮（那扇门不归门户管）', async () => {
    await bootReadOnly();
    assert.equal(screen.queryByLabelText('重命名会话'), null);
    assert.equal(screen.queryByLabelText('归档会话'), null);
  });

  it('只读会话按 after_seq 增量轮询（SSE 不推它）', async () => {
    const { calls } = await bootReadOnly();
    const polls = (): string[] =>
      calls.filter((c) => c.url.includes('qq%3Agroup%3A67890') && c.url.startsWith('/api/history')).map((c) => c.url);

    // 首屏（切会话时）已拉过一次全量。
    await waitFor(() => expect(polls().length).toBeGreaterThan(0));
    await waitFor(() => expect(polls().some((u) => u.includes('after_seq=12'))).toBe(true), { timeout: 6000 });
    // 增量而非重拉：带的是已见的最大 seq，且带上 all=1（按任务分段的整段会话）。
    const incremental = polls().filter((u) => u.includes('after_seq=12'));
    assert.ok(incremental.length > 0);
    assert.ok(incremental.every((u) => u.includes('all=1')));
  });
});
