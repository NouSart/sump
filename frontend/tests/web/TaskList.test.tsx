// TaskList：会话 → 任务的目录。有记录可跳；进行中但没记录的可恢复；已完结没记录的禁用。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { render, screen, fireEvent } from '@testing-library/react';
import { TaskList } from '../../src/web/components/TaskList';
import type { Translate } from '../../src/web/i18n';
import type { TaskView } from '../../src/web/types';

const tZh: Translate = (key, fallback) => fallback ?? key;

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
  return render(<TaskList tasks={[active, done]} jumps={new Set<string>()} current="t-active" onJump={() => {}} t={tZh} {...over} />);
}

describe('TaskList', () => {
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
