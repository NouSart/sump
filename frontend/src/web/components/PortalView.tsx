import { Fragment, useEffect, useRef, useState } from 'react';
import type { PendingApprovalView, PortalMessage, TaskView, TerminalRunUpdate, TimerView } from '../types';
import type { UploadResult } from '../api';
import { ApprovalStrip } from './ApprovalStrip';
import type { Translate } from '../i18n';
import { IconAnswer, IconOutput, IconPaperclip, IconSend, IconSliders, IconSparkle, IconTerminal, IconUser } from './icons';
import { Markdown } from './markdown';
import { TerminalDock } from './TerminalDock';
import { useTraceOpen, type WorkflowExpand } from '../workflowExpand';

interface Props {
  connected: boolean;
  messages: PortalMessage[];
  timers: TimerView[];
  approvals: PendingApprovalView[];
  onDecide: (itemId: string, decision: 'allow' | 'deny') => Promise<void>;
  error: string | null;
  /** 发送消息；返回是否被受理（false 时输入框保草稿）。附件随消息一起发出。 */
  onSend: (text: string, attachments: UploadResult[]) => Promise<boolean>;
  /** 上传一个文件（门户转存到 files / images）；失败抛 Error，消息即提示。 */
  onUpload: (file: File) => Promise<UploadResult>;
  /** 终端里执行一条命令；返回受理句柄。 */
  onTerminal: (command: string) => Promise<{ itemId: string }>;
  /** 终端事件（SSE 推来）：按 itemId 归到对应命令上。 */
  terminalRuns: TerminalRunUpdate[];
  onOpenSettings: () => void;
  /** 任务清单：面板按任务分节，节头要用它的名字与状态。 */
  tasks: TaskView[];
  /** 上一轮推理失败、任务还开着：显示「重试」。 */
  retryable?: boolean;
  onRetry?: () => Promise<void>;
  /** 工作流展开档位：决定过程条目（思考 / 输出）什么时候自己摊开。 */
  workflowExpand: WorkflowExpand;
  /** 只读：这是别的入口（QQ）的会话——能看记录，不能在这里说话。 */
  readOnly?: boolean;
  t: Translate;
}

// 倒计时读数：只在有计时的时候每秒重绘（没计时就不跑定时器）。
function useTick(active: boolean): number {
  const [now, setNow] = useState(() => Date.now());
  useEffect(() => {
    if (!active) return;
    const id = setInterval(() => {
      setNow(Date.now());
    }, 1000);
    return () => {
      clearInterval(id);
    };
  }, [active]);
  return now;
}

// humanSize 把字节数写成面板上的读数（与上传上限提示同一口径）。
function humanSize(n: number): string {
  if (n < 1024) return `${String(n)} B`;
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`;
  return `${(n / (1024 * 1024)).toFixed(1)} MB`;
}

// remainingText 从共用模块取用并重导出：本组件与测试都从这里取同一个实现。
import { remainingText } from '../timeText';
export { remainingText };
// Step 是对话里的一步：左边一个图标 + 竖线（把相邻图标连起来），右边是这一步的内容。
// 图标按步骤类型分：我 / 深度思考 / 命令 / 输出 / 答复——一眼能看出这条链走到哪了。
function Step({ m, t, workflowExpand }: { m: PortalMessage; t: Translate; workflowExpand: WorkflowExpand }) {
  const time = m.at === '' ? '' : m.at.slice(11, 19);
  const kind = m.kind ?? (m.author === 'user' ? 'user' : 'assistant');

  if (kind === 'user') {
    return (
      <li className="step step-user">
        <span className="step-mark">
          <IconUser />
        </span>
        <div className="step-body">
          <div className="step-head">
            <span className="step-title">{t('ui.me.name', '本机用户')}</span>
            <time className="step-time">{time}</time>
          </div>
          <p className="step-text">{m.text}</p>
        </div>
      </li>
    );
  }

  if (kind === 'reasoning') {
    return (
      <li className="step step-reasoning">
        <span className="step-mark">
          <IconSparkle />
        </span>
        <div className="step-body">
          <TraceDetails mode={workflowExpand}>
            <summary className="step-head">
              <span className="step-title">{t('ui.trace.thinking', '深度思考')}</span>
              <span className="step-preview">{firstLine(m.text)}</span>
              <time className="step-time">{time}</time>
            </summary>
            <p className="trace-text">{m.text}</p>
          </TraceDetails>
        </div>
      </li>
    );
  }

  if (kind === 'tool_call') {
    return (
      <li className="step step-call">
        <span className="step-mark">
          <IconTerminal />
        </span>
        <div className="step-body">
          <div className="step-head">
            <span className="step-title">{m.meta?.tool ?? t('ui.trace.command', '命令')}</span>
            <code className="step-cmd">{m.text}</code>
            {m.meta?.truncated === true && <span className="step-note">{t('ui.trace.truncated', '已截断')}</span>}
            <time className="step-time">{time}</time>
          </div>
        </div>
      </li>
    );
  }

  if (kind === 'tool_result') {
    const lines = m.text.split('\n').length;
    return (
      <li className="step step-result">
        <span className="step-mark">
          <IconOutput />
        </span>
        <div className="step-body">
          <TraceDetails mode={workflowExpand}>
            <summary className="step-head">
              <span className="step-title">{m.meta?.tool ?? t('ui.trace.output', '终端输出')}</span>
              <span className="step-preview">
                {String(lines)} {t('ui.trace.lines', '行')}
                {m.meta?.truncated === true ? ` · ${t('ui.trace.truncated', '已截断')}` : ''}
              </span>
              <time className="step-time">{time}</time>
            </summary>
            <pre className="trace-pre">{m.text}</pre>
          </TraceDetails>
        </div>
      </li>
    );
  }

  // 最终答复：图标收口，正文按 Markdown 渲染（代码块、列表、表格都画出来）。
  return (
    <li className="step step-answer">
      <span className="step-mark">
        <IconAnswer />
      </span>
      <div className="step-body">
        <div className="step-head">
          <span className="step-title">SUMP</span>
          <time className="step-time">{time}</time>
        </div>
        <Markdown text={m.text} />
      </div>
    </li>
  );
}

// firstLine 取第一行当摘要（思考往往很长，先给一句）。
function firstLine(text: string): string {
  const line = text.split('\n').find((l) => l.trim() !== '') ?? '';
  return line.length > 80 ? `${line.slice(0, 80)}…` : line;
}

// TraceDetails 是可折叠的过程条目（深度思考 / 终端输出），行为由"工作流展开"档位决定：
//  - 折叠：默认收起，用户点 summary 自己开；
//  - 展开：始终展开；
//  - 焦点展开：悬停或键盘聚焦时展开，移开/失焦收起——但一旦用户亲手点开过，
//    就以用户的意志为准，不再跟着鼠标自动收起（否则刚点开就被抢着关掉，很恼人）。
// 用受控 open 而不是 CSS：<details> 的展开是浏览器行为，样式表改不了 open 状态。
function TraceDetails({ mode, children }: { mode: WorkflowExpand; children: React.ReactNode }) {
  const detail = useTraceOpen(mode);
  return (
    <details className="step-details" open={detail.open} onToggle={detail.onToggle} {...detail.handlers}>
      {children}
    </details>
  );
}
// PortalView 是门户的对话面：消息流铺满整个面板，输入区（含待审/计时器/报错）作为浮层压在底部——
// 所以输入框是悬浮的，消息能滚到它背后透出来。消息只有两条来源：本地回显，与 SSE 送来的 deliver。
export function PortalView({ connected, messages, timers, approvals, onDecide, error, onSend, onUpload, onTerminal, terminalRuns, onOpenSettings, tasks, workflowExpand, retryable, onRetry, readOnly, t }: Props) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  // 附件：上传成功的留在列表里，随下一条消息一起发出去；上传中/失败各自有态。
  const [attachments, setAttachments] = useState<UploadResult[]>([]);
  const [uploading, setUploading] = useState(false);
  const [uploadError, setUploadError] = useState<string | null>(null);
  const fileInputRef = useRef<HTMLInputElement>(null);
  // 终端 = 放大的输入框：开关只影响这一页，退出即收起。
  const [terminalOpen, setTerminalOpen] = useState(false);
  // 输入框未聚焦时不显示「说点什么…」：占位提示只在真正要打字时出现。
  const [focused, setFocused] = useState(false);
  const listRef = useRef<HTMLOListElement>(null);
  const dockRef = useRef<HTMLDivElement>(null);
  // 浮层高度会变（待审卡片、计时器、终端都会长），量出来挂到 documentElement 上：
  // 消息列表的底部留白与右侧消息卡片都要用它，而消息卡片已经不在 .portal 里了。
  const now = useTick(timers.length > 0);

  useEffect(() => {
    const el = dockRef.current;
    if (el === null || typeof ResizeObserver === 'undefined') return;
    const ro = new ResizeObserver(() => {
      // 挂在 documentElement 上：消息卡片挂在 .app 下，也要用同一个浮层高度。
      document.documentElement.style.setProperty('--dock-space', `${String(el.offsetHeight)}px`);
    });
    ro.observe(el);
    document.documentElement.style.setProperty('--dock-space', `${String(el.offsetHeight)}px`);
    return () => {
      ro.disconnect();
      document.documentElement.style.removeProperty('--dock-space');
    };
  }, []);

  // 新消息落下后跟随到底部——但只在用户本来就在底部时跟随：
  // 往上翻历史时被 progress 一次次拽回底部，比看不到新消息更糟。
  // 离开底部后给一个「最新」按钮，想回来自觉点一下。
  const [stick, setStick] = useState(true);
  const nearBottom = (list: HTMLOListElement): boolean =>
    list.scrollHeight - list.scrollTop - list.clientHeight <= 80;
  const jumpToLatest = (): void => {
    const list = listRef.current;
    setStick(true);
    if (list !== null && typeof list.scrollTo === 'function') list.scrollTo({ top: list.scrollHeight });
  };
  useEffect(() => {
    if (!stick) return;
    const list = listRef.current;
    if (list !== null && typeof list.scrollTo === 'function') list.scrollTo({ top: list.scrollHeight });
  }, [messages.length, stick]);

  const submit = async (e: { preventDefault: () => void }): Promise<void> => {
    e.preventDefault();
    const value = text.trim();
    if (busy || uploading) return;
    if (value === '' && attachments.length === 0) return;
    setBusy(true);
    setStick(true);
    try {
      // 只有被受理才清空：没送出去的文字与附件要留给用户重试。
      if (await onSend(value, attachments)) {
        setText('');
        setAttachments([]);
        setUploadError(null);
      }
    } finally {
      setBusy(false);
    }
  };

  // pickFiles 上传选中的文件：逐个来（单个失败不牵连其它），全部结束后清空 input，
  // 否则同一个文件选第二次不会触发 change。
  const pickFiles = async (files: FileList | null): Promise<void> => {
    if (files === null || files.length === 0) return;
    setUploading(true);
    setUploadError(null);
    try {
      for (const file of Array.from(files)) {
        try {
          const saved = await onUpload(file);
          setAttachments((prev) => [...prev, saved]);
        } catch (err) {
          setUploadError(`${file.name}: ${err instanceof Error ? err.message : String(err)}`);
        }
      }
    } finally {
      setUploading(false);
      if (fileInputRef.current !== null) fileInputRef.current.value = '';
    }
  };

  const dropAttachment = (id: string): void => {
    setAttachments((prev) => prev.filter((a) => a.id !== id));
  };

  return (
    <section
      className="portal"
      aria-label={t('ui.portal.title', '对话')}
    >
      <ol
        className="portal-list"
        ref={listRef}
        onScroll={(e) => {
          const next = nearBottom(e.currentTarget);
          setStick((prev) => (prev === next ? prev : next));
        }}
      >
        {messages.length === 0 ? (
          <li className="portal-empty">{t('ui.portal.empty', '发一条消息开始。回复会经 deliver 实时送回来。')}</li>
        ) : (
          messages.map((m, i) => {
            const prev = i > 0 ? messages[i - 1] : undefined;
            const newTask = m.taskId !== undefined && m.taskId !== '' && m.taskId !== prev?.taskId;
            const task = newTask ? tasks.find((x) => x.id === m.taskId) : undefined;
            return (
              <Fragment key={m.id ?? (m.seq !== undefined && m.seq > 0 ? `s${String(m.seq)}` : `i${String(i)}`)}>
                {newTask && (
                  <li className="task-sep" id={`task-${m.taskId ?? ''}`}>
                    <span className="task-sep-name">{task?.name ?? t('ui.task.label', '任务')}</span>
                    <span className="task-sep-state">
                      {task?.status === 'done' ? t('ui.task.done', '已完成') : t('ui.task.active', '进行中')}
                    </span>
                  </li>
                )}
                <Step m={m} t={t} workflowExpand={workflowExpand} />
              </Fragment>
            );
          })
        )}
      </ol>

      <div className="dock" ref={dockRef}>
        {!stick && messages.length > 0 && (
          <div className="jump-row">
            <button type="button" className="jump-latest" onClick={jumpToLatest}>
              {t('ui.portal.latest', '↓ 回到最新')}
            </button>
          </div>
        )}
        {error !== null && (
          <p className="inline-error" role="alert">
            {error}
          </p>
        )}

        {/* 待审命令：白名单没覆盖的命令挂起在这里，等用户点批准或拒绝。
            放在输入框正上方——它是"需要你现在做决定"的东西，不能滚出视野。 */}
        <ApprovalStrip items={approvals} onDecide={onDecide} t={t} />

        {/* 在计时器：timer 推来全量快照，这里只负责画读数。
            到点后不清屏——timer 会推一条不含它的快照过来；快照还没到就显示「到点…」，
            绝不假装"没有计时"。 */}
        {timers.length > 0 && (
          <ol className="timer-strip" aria-label={t('ui.portal.timers', '在计时器')}>
            {timers.map((tm) => {
              const left = new Date(tm.fireAt).getTime() - now;
              return (
                <li key={tm.itemId} className="timer-item">
                  <span className="timer-left">{left <= 0 ? t('ui.portal.timerDue', '到点…') : `${remainingText(left)} ${t('ui.portal.timerWake', '后唤醒')}`}</span>
                  {tm.note !== '' && <span className="timer-note">{tm.note}</span>}
                </li>
              );
            })}
          </ol>
        )}

        <form
          className={terminalOpen ? 'composer composer-terminal' : 'composer'}
          onSubmit={(e) => {
            void submit(e);
          }}
        >
          {/* 只读会话（QQ）：记录能看，话要回 QQ 说——门户不替别的入口发言。 */}
          {readOnly === true && (
            <div className="composer-readonly" role="status">
              <span>{t('ui.portal.readonly', '只读查看：这是 QQ 里的对话，回到 QQ 里回复')}</span>
            </div>
          )}
          {/* 上一轮推理失败但任务还开着：给一个明确的「重试」，不用用户自己想办法触发。 */}
          {retryable === true && onRetry !== undefined && (
            <div className="composer-retry" role="status">
              <span>{t('ui.portal.roundFailed', '上一轮推理失败，任务还在')}</span>
              <button
                type="button"
                className="composer-retry-btn"
                onClick={() => {
                  void onRetry();
                }}
              >
                {t('ui.portal.retry', '重试')}
              </button>
            </div>
          )}
          {/* 终端就长在输入框里：展开后上半部是终端，底部仍是给 agent 的消息框。 */}
          <TerminalDock
            open={terminalOpen}
            onClose={() => {
              setTerminalOpen(false);
            }}
            onRun={onTerminal}
            updates={terminalRuns}
            t={t}
          />
          {/* 附件条：上传中的文件、已上传的 chip（可删）、以及就地显示的错误。
              放在输入框上方，跟消息一起发出去；不弹 toast——失败就停在原地，用户能重试。 */}
          {(attachments.length > 0 || uploading || uploadError !== null) && (
            <div className="composer-attachments" role="status">
              {attachments.map((a) => (
                <span key={a.id} className="attach-chip" title={`${a.kind === 'image' ? 'images' : 'files'}:${a.id}`}>
                  <span className="attach-name">{a.filename}</span>
                  <span className="attach-size">{humanSize(a.size)}</span>
                  <button
                    type="button"
                    className="attach-remove"
                    aria-label={`${t('ui.portal.attachRemove', '移除附件')}: ${a.filename}`}
                    onClick={() => {
                      dropAttachment(a.id);
                    }}
                  >
                    ×
                  </button>
                </span>
              ))}
              {uploading && <span className="attach-chip is-busy">{t('ui.portal.attachUploading', '上传中…')}</span>}
              {uploadError !== null && <span className="attach-error">{uploadError}</span>}
            </div>
          )}
          <textarea
            className={terminalOpen ? 'composer-input is-compact' : 'composer-input'}
            aria-label={t('ui.portal.input', '消息')}
            rows={terminalOpen ? 1 : 2}
            value={readOnly === true ? '' : text}
            placeholder={
              readOnly === true
                ? t('ui.portal.readonlyPlaceholder', '只读查看（回复请去 QQ）')
                : !connected
                  ? t('ui.portal.offline', '未连接设置中心')
                  : focused
                    ? t('ui.portal.placeholder', '说点什么…（Enter 发送，Shift+Enter 换行）')
                    : ''
            }
            disabled={!connected || readOnly === true}
            onFocus={() => {
              setFocused(true);
            }}
            onBlur={() => {
              setFocused(false);
            }}
            onChange={(e) => {
              setText(e.target.value);
            }}
            onKeyDown={(e) => {
              if (e.key === 'Enter' && !e.shiftKey) {
                e.preventDefault();
                void submit(e);
              }
            }}
          />
          <div className="composer-row">
            <button type="button" className="chip" onClick={onOpenSettings} aria-label={t('ui.settings.title', '设置')} title={t('ui.settings.title', '设置')}>
              <IconSliders />
            </button>
            <button
              type="button"
              className="chip"
              disabled={!connected || uploading || readOnly === true}
              aria-label={t('ui.portal.attach', '添加附件')}
              title={t('ui.portal.attach', '添加附件')}
              onClick={() => {
                fileInputRef.current?.click();
              }}
            >
              <IconPaperclip />
            </button>
            <input
              ref={fileInputRef}
              type="file"
              multiple
              hidden
              aria-label={t('ui.portal.attach', '添加附件')}
              onChange={(e) => {
                void pickFiles(e.target.files);
              }}
            />
            <button
              type="button"
              className={terminalOpen ? 'chip is-on' : 'chip'}
              aria-pressed={terminalOpen}
              disabled={readOnly === true}
              onClick={() => {
                setTerminalOpen((value) => !value);
              }}
              aria-label={t('ui.portal.terminal', '终端')}
              title={t('ui.portal.terminal', '终端')}
            >
              <IconTerminal />
            </button>
            <span className="grow" />
            <button
              type="submit"
              className="chip primary"
              aria-label={t('ui.portal.send', '发送')}
              aria-busy={busy}
              title={t('ui.portal.sendHint', '发送（Enter）')}
              disabled={!connected || readOnly === true || busy || uploading || (text.trim() === '' && attachments.length === 0)}
            >
              <IconSend />
            </button>
          </div>
        </form>
      </div>
    </section>
  );
}
