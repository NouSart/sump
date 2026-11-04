// TerminalDock 是"放大的输入框"：向上向左右伸长，变成与 agent 共享的终端。
//
// 命令是真的执行：这里只发 /api/terminal，由门户交给 shell（boss=门户），shell 照样走审批；
// 结果经 report_result → 门户 SSE → 本组件。所以这里**不做任何裁决**，只负责显示：
// 提交了什么、在等审批还是在跑、输出了什么、退出码是多少。
// 它不是 PTY：交互程序（vim / top）跑不了，也没有 Ctrl+C——shell 只有 run，没有 cancel。
import { useEffect, useRef, useState, type ReactElement } from 'react';
import { IconClose, IconTerminal } from './icons';
import type { Translate } from '../i18n';
import type { TerminalRunUpdate } from '../types';

interface Props {
  open: boolean;
  onClose: () => void;
  t: Translate;
  /** 执行一条命令；返回受理句柄（itemId）。失败抛 Error（消息即提示）。 */
  onRun: (command: string) => Promise<{ itemId: string }>;
  /** 终端事件（SSE 推来）：按 itemId 归到对应命令上。 */
  updates: TerminalRunUpdate[];
}

interface Line {
  kind: 'cmd' | 'note' | 'out' | 'err' | 'exit';
  text: string;
  itemId?: string;
}

// updateLines 把一条 shell 补报翻成终端里的行：审批结果、stdout / stderr、退出码。
function updateLines(u: TerminalRunUpdate, t: Translate): Line[] {
  const out: Line[] = [];
  if (u.phase === 'approved') {
    out.push({ kind: 'note', text: t('ui.terminal.approved', '审批通过，开始执行'), itemId: u.itemId });
    return out;
  }
  if (u.phase === 'rejected') {
    out.push({ kind: 'err', text: u.error !== '' ? u.error : u.text, itemId: u.itemId });
    return out;
  }
  if (!u.final) return out;
  if (u.stdout !== '') out.push({ kind: 'out', text: u.stdout, itemId: u.itemId });
  if (u.stderr !== '') out.push({ kind: 'err', text: u.stderr, itemId: u.itemId });
  if (u.error !== '') out.push({ kind: 'err', text: u.error, itemId: u.itemId });
  const notes: string[] = [];
  if (u.timedOut) notes.push(t('ui.terminal.timedOut', '超时终止'));
  if (u.truncated) notes.push(t('ui.terminal.truncated', '输出已截断'));
  const suffix = notes.length > 0 ? `（${notes.join('，')}）` : '';
  out.push({ kind: 'exit', text: `exit ${String(u.exitCode)}${suffix}`, itemId: u.itemId });
  return out;
}

export function TerminalDock({ open, onClose, t, onRun, updates }: Props): ReactElement | null {
  const [lines, setLines] = useState<Line[]>([]);
  const [draft, setDraft] = useState('');
  const [running, setRunning] = useState(false);
  const [error, setError] = useState<string | null>(null);
  const bodyRef = useRef<HTMLDivElement>(null);
  // 已经画过的补报：门户重启后补发、或多标签页重复推同一条时按 itemId+phase+at 去重。
  const drawnRef = useRef(new Set<string>());

  useEffect(() => {
    if (!open) return;
    const onKey = (e: KeyboardEvent): void => {
      if (e.key === 'Escape') onClose();
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  }, [open, onClose]);

  useEffect(() => {
    const body = bodyRef.current;
    if (body !== null && typeof body.scrollTo === 'function') body.scrollTo({ top: body.scrollHeight });
  }, [lines.length]);

  // 补报到达 → 追加成新的行（只追加、不改写历史行，顺序即到达顺序）。
  useEffect(() => {
    const fresh: Line[] = [];
    for (const u of updates) {
      const key = `${u.itemId}:${u.phase}:${u.at}`;
      if (drawnRef.current.has(key)) continue;
      drawnRef.current.add(key);
      fresh.push(...updateLines(u, t));
    }
    if (fresh.length > 0) setLines((prev) => [...prev, ...fresh]);
  }, [updates, t]);

  if (!open) return null;

  const submit = async (e: { preventDefault: () => void }): Promise<void> => {
    e.preventDefault();
    const value = draft.trim();
    if (value === '' || running) return;
    setDraft('');
    setError(null);
    setRunning(true);
    try {
      const { itemId } = await onRun(value);
      setLines((prev) => [
        ...prev,
        { kind: 'cmd', text: value, itemId },
        { kind: 'note', text: t('ui.terminal.queued', '已提交，等待审批或执行…'), itemId },
      ]);
    } catch (err) {
      // 没提交成功就别假装执行了：把命令放回输入框，让用户改或重试。
      setDraft(value);
      setError(err instanceof Error ? err.message : String(err));
    } finally {
      setRunning(false);
    }
  };

  return (
    <section className="terminal-pane" aria-label={t('ui.terminal.title', '终端')}>
      <header className="terminal-head">
        <span className="terminal-glyph" aria-hidden="true">
          <IconTerminal />
        </span>
        <span className="terminal-title">{t('ui.terminal.title', '终端')}</span>
        <span className="terminal-hint">{t('ui.terminal.hint', '命令经审批后执行 · 不是交互终端')}</span>
        <span className="grow" />
        <button
          type="button"
          className="chip"
          onClick={onClose}
          aria-label={t('ui.terminal.close', '关闭终端')}
          title={t('ui.terminal.close', '关闭终端')}
        >
          <IconClose />
        </button>
      </header>
      <div className="terminal-body" ref={bodyRef} role="log" aria-live="polite">
        {lines.length === 0 ? (
          <p className="terminal-empty">{t('ui.terminal.empty', '终端还没有输出。输入一条命令试试。')}</p>
        ) : (
          lines.map((line, index) => (
            <p
              key={`${String(index)}-${line.kind}-${line.text}`}
              className={
                line.kind === 'cmd'
                  ? 'terminal-line'
                  : line.kind === 'err'
                    ? 'terminal-line is-err'
                    : line.kind === 'exit'
                      ? 'terminal-line is-exit'
                      : 'terminal-line is-note'
              }
            >
              {line.kind === 'cmd' ? <span className="terminal-prompt" aria-hidden="true">$ </span> : null}
              {line.text}
            </p>
          ))
        )}
      </div>
      {error !== null && (
        <p className="terminal-error" role="alert">
          {error}
        </p>
      )}
      {/* 提示符输入：不能是嵌套 form（外层已经是给 agent 的消息 form）。 */}
      <div className="terminal-form">
        <span className="terminal-prompt" aria-hidden="true">$</span>
        <input
          className="terminal-input"
          aria-label={t('ui.terminal.input', '终端命令')}
          value={draft}
          placeholder={t('ui.terminal.placeholder', '输入命令…')}
          disabled={running}
          onChange={(e) => {
            setDraft(e.target.value);
          }}
          onKeyDown={(e) => {
            if (e.key !== 'Enter') return;
            e.preventDefault();
            void submit(e);
          }}
        />
      </div>
    </section>
  );
}
