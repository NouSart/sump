// TaskList 是左边的会话树：一条会话，展开是它的任务。
//
// 面板按**会话**展示（整段对话，按任务分节），所以这里点任务不是"切换视图"，而是**跳转**——
// 把面板滚到那一段。任务名就是历史记录的标题。
import { useState } from 'react';
import type { TaskView } from '../types';
import type { Translate } from '../i18n';
import { IconChevronDown, IconChevronRight } from './icons';

interface Props {
  tasks: TaskView[];
  /** 面板里确实有记录的任务（没记录的任务行不点：点了也没地方落）。 */
  jumps: ReadonlySet<string>;
  /** 当前任务 id（= 最近活跃的进行中任务，下一条消息会续在它后面）。 */
  current: string;
  onJump: (taskId: string) => void;
  t: Translate;
}

export function TaskList({ tasks, jumps, current, onJump, t }: Props): React.ReactElement {
  const [open, setOpen] = useState(true);
  // 状态用"当前任务"来标：只有它会被下一条消息续上。
  const currentID = current;

  return (
    <nav className="threads" aria-label={t('ui.task.nav', '会话与任务')}>
      <p className="section-label">
        {t('ui.app.conversations', '会话')}
        {tasks.length > 0 ? ` · ${String(tasks.length)}` : ''}
      </p>
      <button
        type="button"
        className="thread conv"
        aria-expanded={open}
        title={t('ui.app.currentThread', '当前会话')}
        onClick={() => {
          setOpen((v) => !v);
        }}
      >
        {open ? <IconChevronDown /> : <IconChevronRight />}
        <span className="t-title">{t('ui.app.currentThread', '当前会话')}</span>
        <span className="t-time">{currentID === '' ? '' : t('ui.task.active', '进行中')}</span>
      </button>

      {open &&
        (tasks.length === 0 ? (
          <p className="empty-side">{t('ui.task.empty', '还没有任务——发一条消息就会开一个')}</p>
        ) : (
          tasks.map((task) => {
            const done = task.status === 'done';
            const hasRecords = jumps.has(task.id);
            // 每条任务都能点：有记录就跳过去，没有就把它这一段取回来（进行中的先恢复）。
            const title = hasRecords
              ? done
                ? `${task.name} · ${t('ui.task.done', '已完成')}`
                : `${task.name}${task.id === currentID ? ` · ${t('ui.task.current', '当前任务')}` : ''}`
              : `${task.name} · ${done ? t('ui.task.load', '载入记录') : t('ui.task.resume', '恢复这个任务并载入记录')}`;
            return (
              <button
                key={task.id}
                type="button"
                className={`thread task${done ? ' is-done' : ''}${hasRecords ? '' : ' is-unloaded'}${task.id === currentID ? ' is-current' : ''}`}
                title={title}
                onClick={() => {
                  onJump(task.id);
                }}
              >
                <span className={`task-dot${done ? ' is-done' : ''}`} aria-hidden="true" />
                <span className="t-title">{task.name}</span>
                <span className="t-time">{done ? task.updated_at.slice(11, 16) : t('ui.task.active', '进行中')}</span>
              </button>
            );
          })
        ))}
    </nav>
  );
}
