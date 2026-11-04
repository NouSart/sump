// TaskList 是左边的会话树：最外层是会话，展开的是当前会话的任务。
//
// 面板按**会话**展示（整段对话，按任务分节），所以点任务不是"切换视图"，而是**跳转**——
// 把面板滚到那一段。任务名就是历史记录的标题。
//
// 只有当前会话的任务会列出来：别的会话的任务没有载入，列出来的空行点不开。
// 切会话时才去取它的任务与记录（App 负责）。
import { useState } from 'react';
import type { ConversationView, TaskView } from '../types';
import type { Translate } from '../i18n';
import { IconChevronDown, IconChevronRight } from './icons';

interface Props {
  conversations: ConversationView[];
  /** 当前会话 id（空 = 还没选，界面照常只画会话行）。 */
  active: string;
  tasks: TaskView[];
  /** 面板里确实有记录的任务（没记录的任务行不点：点了也没地方落）。 */
  jumps: ReadonlySet<string>;
  /** 当前任务 id（= 最近活跃的进行中任务，下一条消息会续在它后面）。 */
  current: string;
  onSelectConversation: (id: string) => void;
  onCreateConversation: () => void;
  onRenameConversation: (id: string, title: string) => void;
  onArchiveConversation: (id: string) => void;
  onJump: (taskId: string) => void;
  t: Translate;
}

// conversationFallback 给没有名字的会话一个人话标签。
// 优先按入口与会话号推（qq:group:67890 → QQ 群 67890），推不出来才说"未命名会话"——
// QQ 会话的名字由 qq 服务在第一次见到时补上，老数据可能还没名字。
function conversationFallback(conv: ConversationView, t: Translate): string {
  const parts = conv.id.split(':');
  if (parts.length === 3 && parts[0] === 'qq') {
    const who = parts[1] === 'group' ? 'QQ 群' : 'QQ 私聊';
    return `${who} ${parts[2]}`;
  }
  return t('ui.conv.untitled', '未命名会话');
}

export function TaskList({
  conversations,
  active,
  tasks,
  jumps,
  current,
  onSelectConversation,
  onCreateConversation,
  onRenameConversation,
  onArchiveConversation,
  onJump,
  t,
}: Props): React.ReactElement {
  const [open, setOpen] = useState(true);
  const currentID = current;
  const activeConversation = conversations.find((c) => c.id === active) ?? null;

  const rename = (conv: ConversationView): void => {
    const next = window.prompt(t('ui.conv.renamePrompt', '会话名（留空 = 不命名）'), conv.title);
    if (next === null) return;
    onRenameConversation(conv.id, next.trim());
  };

  return (
    <nav className="threads" aria-label={t('ui.task.nav', '会话与任务')}>
      <p className="section-label">
        {t('ui.app.conversations', '会话')}
        {conversations.length > 0 ? ` · ${String(conversations.length)}` : ''}
        <button
          type="button"
          className="icon-btn"
          aria-label={t('ui.conv.new', '新会话')}
          title={t('ui.conv.new', '新会话')}
          onClick={onCreateConversation}
        >
          +
        </button>
      </p>

      {conversations.length === 0 ? (
        <p className="empty-side">{t('ui.conv.empty', '还没有会话——新建一个或直接发消息')}</p>
      ) : (
        conversations.map((conv) => {
          const isActive = conv.id === active;
          const name = conv.title === '' ? conversationFallback(conv, t) : conv.title;
          // 别的入口（QQ）的会话只能看：改名 / 归档 / 发消息都归那扇门自己管。
          const foreign = conv.readonly === true;
          return (
            <div key={conv.id} className="conv-group">
              <button
                type="button"
                className={`thread conv${isActive ? ' is-current' : ''}${foreign ? ' is-foreign' : ''}`}
                aria-expanded={isActive ? open : undefined}
                // 读屏与测试都靠它认出"哪个是当前会话"（会话名本身可能是空的）。
                aria-label={isActive ? `${name} · ${t('ui.app.currentThread', '当前会话')}` : name}
                title={isActive ? t('ui.app.currentThread', '当前会话') : name}
                onClick={() => {
                  if (isActive) {
                    setOpen((v) => !v);
                    return;
                  }
                  onSelectConversation(conv.id);
                }}
              >
                {isActive && open ? (
                  <IconChevronDown />
                ) : isActive ? (
                  <IconChevronRight />
                ) : (
                  <span className="task-dot" aria-hidden="true" />
                )}
                <span className="t-title">{name}</span>
                <span className="t-time">
                  {foreign ? t('ui.conv.readonlyTag', '只读') : conv.updated_at === '' ? '' : conv.updated_at.slice(5, 16)}
                </span>
              </button>
              {isActive && !foreign && (
                <span className="conv-actions">
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label={t('ui.conv.rename', '重命名会话')}
                    title={t('ui.conv.rename', '重命名会话')}
                    onClick={() => {
                      rename(conv);
                    }}
                  >
                    ✎
                  </button>
                  <button
                    type="button"
                    className="icon-btn"
                    aria-label={t('ui.conv.archive', '归档会话')}
                    title={t('ui.conv.archive', '归档会话')}
                    onClick={() => {
                      onArchiveConversation(conv.id);
                    }}
                  >
                    ⌫
                  </button>
                </span>
              )}
            </div>
          );
        })
      )}

      {activeConversation !== null && open && (
        <>
          {tasks.length === 0 ? (
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
          )}
        </>
      )}
    </nav>
  );
}
