// TerminalDock 是"放大的输入框"：向上、向左右伸长，变成一个与 agent 共享的终端。
// 本轮只做 UI——命令不会真的执行，输入只在本地回显；底部给 agent 的消息框照常可用。
import { useEffect, useRef, useState, type ReactElement } from 'react';
import { IconClose, IconTerminal } from './icons';
import type { Translate } from '../i18n';

interface Props {
  open: boolean;
  onClose: () => void;
  t: Translate;
}

interface Line {
  kind: 'cmd' | 'note';
  text: string;
}

export function TerminalDock({ open, onClose, t }: Props): ReactElement | null {
  const [lines, setLines] = useState<Line[]>([]);
  const [draft, setDraft] = useState('');
  const bodyRef = useRef<HTMLDivElement>(null);

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

  if (!open) return null;

  const submit = (e: { preventDefault: () => void }): void => {
    e.preventDefault();
    const value = draft.trim();
    if (value === '') return;
    setDraft('');
    setLines((prev) => [
      ...prev,
      { kind: 'cmd', text: value },
      { kind: 'note', text: t('ui.terminal.uiOnly', '（UI 预览：终端尚未接入 shell，命令没有执行）') },
    ]);
  };
  return (
    <section className="terminal-pane" aria-label={t('ui.terminal.title', '终端')}>
      <header className="terminal-head">
        <span className="terminal-glyph" aria-hidden="true">
          <IconTerminal />
        </span>
        <span className="terminal-title">{t('ui.terminal.title', '终端')}</span>
        <span className="terminal-hint">{t('ui.terminal.uiOnlyShort', 'UI 预览 · 命令不会执行')}</span>
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
          <p className="terminal-empty">{t('ui.terminal.empty', '终端还没有输出。输入一条命令试试（本轮只回显）。')}</p>
        ) : (
          lines.map((line, index) => (
            <p key={`${String(index)}-${line.text}`} className={line.kind === 'cmd' ? 'terminal-line' : 'terminal-line is-note'}>
              {line.kind === 'cmd' ? <span className="terminal-prompt" aria-hidden="true">$ </span> : null}
              {line.text}
            </p>
          ))
        )}
      </div>
      {/* 提示符输入：不能是嵌套 form（外层已经是给 agent 的消息 form）。 */}
      <div className="terminal-form">
        <span className="terminal-prompt" aria-hidden="true">$</span>
        <input
          className="terminal-input"
          aria-label={t('ui.terminal.input', '终端命令')}
          value={draft}
          placeholder={t('ui.terminal.placeholder', '输入命令…')}
          onChange={(e) => {
            setDraft(e.target.value);
          }}
          onKeyDown={(e) => {
            if (e.key !== 'Enter') return;
            e.preventDefault();
            submit(e);
          }}
        />
      </div>
    </section>
  );
}
