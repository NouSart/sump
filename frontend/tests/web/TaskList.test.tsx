// TaskList：会话 → 任务的目录。
// 第一层是会话（可切换 / 新建 / 改名 / 归档），第二层是当前会话的任务
// （有记录可跳；进行中但没记录的可恢复；已完结没记录的按 task_id 载入）。
import assert from 'node:assert/strict';
import { describe, it, expect, vi, afterEach } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { TaskList } from '../../src/web/components/TaskList';
import type { Translate } from '../../src/web/i18n';
import type { ConversationView, TaskView } from '../../src/web/types';

const tZh: Translate = (key, fallback) => fallback ?? key;

const conv: ConversationView = {
  id: 'web:private:owner',
  title: '主会话',
  entry: 'web',
  status: 'active',
  created_at: '2026-10-17T10:00:00Z',
  updated_at: '2026-10-17T10:05:00Z',
};
const other: ConversationView = {
  ...conv,
  id: 'web:private:owner-x1',
  title: '另一个会话',
  updated_at: '2026-10-16T09:00:00Z',
};
const untitled: ConversationView = { ...conv, id: 'web:private:owner-x2', title: '' };

const active: TaskView = {
  id: 't-active',
  conversation_id: 'web:private:owner',
  name: '进行中的任务',
  status: 'active',
  created_at: '2026-10-17T10:00:00Z',
  updated_at: '2026-10-17T10:05:00Z',
};
const done: TaskView = { ...active, id: 't-done', name: '完结的任务', status: 'done' };

function renderList(over: Partial<Parameters<typeof TaskList>[0]> = {}) {
  return render(
    <TaskList
      conversations={[conv, other]}
      active={conv.id}
      tasks={[active, done]}
      jumps={new Set<string>()}
      current="t-active"
      onSelectConversation={() => {}}
      onCreateConversation={() => {}}
      onRenameConversation={() => {}}
      onArchiveConversation={() => {}}
      onJump={() => {}}
      t={tZh}
      {...over}
    />,
  );
}

afterEach(() => {
  vi.restoreAllMocks();
});

describe('TaskList · 任务', () => {
  it('有记录的任务可点，点了回传 taskId', () => {
    const onJump = vi.fn();
    renderList({ jumps: new Set(['t-active']), onJump });
    fireEvent.click(screen.getByTitle(/^进行中的任务/));
    expect(onJump).toHaveBeenCalledWith('t-active');
  });

  it('记录没载入：可点，且不显示灰态', () => {
    const onJump = vi.fn();
    renderList({ onJump });
    const btn = screen.getByTitle('进行中的任务 · 恢复这个任务并载入记录');
    assert.equal((btn as HTMLButtonElement).disabled, false);
    assert.ok(btn.className.includes('is-unloaded'));
    fireEvent.click(btn);
    expect(onJump).toHaveBeenCalledWith('t-active');
  });

  it('已完结但记录不在窗口内的任务：点一下按 task_id 读历史并载入', () => {
    const onJump = vi.fn();
    renderList({ onJump });
    const btn = screen.getByTitle('完结的任务 · 载入记录');
    assert.equal((btn as HTMLButtonElement).disabled, false);
    fireEvent.click(btn);
    expect(onJump).toHaveBeenCalledWith('t-done');
  });

  it('当前（进行中）任务带标记', () => {
    renderList({ jumps: new Set(['t-active']) });
    assert.ok(screen.getByTitle(/当前任务/).className.includes('is-current'));
  });
});

describe('TaskList · 会话', () => {
  it('列出会话，当前会话展开出任务', () => {
    renderList();
    expect(screen.getByText('主会话')).toBeTruthy();
    expect(screen.getByText('另一个会话')).toBeTruthy();
    expect(screen.getByTitle('当前会话')).toBeTruthy();
    expect(screen.getByText('进行中的任务')).toBeTruthy();
  });

  it('点另一个会话 → 回传它的 id（服务端才是真相，组件不自己切）', () => {
    const onSelectConversation = vi.fn();
    renderList({ onSelectConversation });
    fireEvent.click(screen.getByTitle('另一个会话'));
    expect(onSelectConversation).toHaveBeenCalledWith(other.id);
  });

  it('未命名会话显示占位名', () => {
    renderList({ conversations: [untitled], active: untitled.id, tasks: [] });
    expect(screen.getByText('未命名会话')).toBeTruthy();
  });

  it('新会话按钮回传', () => {
    const onCreateConversation = vi.fn();
    renderList({ onCreateConversation });
    fireEvent.click(screen.getByTitle('新会话'));
    expect(onCreateConversation).toHaveBeenCalledTimes(1);
  });

  it('重命名：确认后回传新名字；取消则什么都不做', () => {
    const onRenameConversation = vi.fn();
    const prompt = vi.spyOn(window, 'prompt');
    renderList({ onRenameConversation });

    prompt.mockReturnValueOnce('改过的名字');
    fireEvent.click(screen.getByTitle('重命名会话'));
    expect(onRenameConversation).toHaveBeenCalledWith(conv.id, '改过的名字');

    onRenameConversation.mockClear();
    prompt.mockReturnValueOnce(null);
    fireEvent.click(screen.getByTitle('重命名会话'));
    expect(onRenameConversation).not.toHaveBeenCalled();
  });

  it('归档按钮回传当前会话', () => {
    const onArchiveConversation = vi.fn();
    renderList({ onArchiveConversation });
    fireEvent.click(screen.getByTitle('归档会话'));
    expect(onArchiveConversation).toHaveBeenCalledWith(conv.id);
  });
});
