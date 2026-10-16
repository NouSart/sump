import { useEffect, useRef, useState } from 'react';
import type { PortalMessage } from '../types';

interface Props {
  connected: boolean;
  messages: PortalMessage[];
  error: string | null;
  onSend: (text: string) => Promise<void>;
}

// PortalView 是门户的对话面：上方消息流，下方输入行。
// 消息只有两条来源——本地回显的输入，与 SSE 送来的 deliver。
export function PortalView({ connected, messages, error, onSend }: Props) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const listRef = useRef<HTMLOListElement>(null);

  // 新消息落下后把视图带到底部（jsdom 没有 scrollTo，测试环境跳过）。
  useEffect(() => {
    const list = listRef.current;
    if (list !== null && typeof list.scrollTo === 'function') list.scrollTo({ top: list.scrollHeight });
  }, [messages.length]);

  const submit = async (e: { preventDefault: () => void }): Promise<void> => {
    e.preventDefault();
    const value = text.trim();
    if (value === '' || busy) return;
    setBusy(true);
    try {
      await onSend(value);
      setText('');
    } finally {
      setBusy(false);
    }
  };

  return (
    <section className="portal" aria-label="门户">
      <h1 className="portal-title">
        门户
        <span className="portal-sub">与智能体对话 · 单用户</span>
      </h1>

      <ol className="portal-list" ref={listRef}>
        {messages.length === 0 ? (
          <li className="portal-empty">发一条消息开始。回复会经 deliver 实时送回来。</li>
        ) : (
          messages.map((m, i) => (
            <li key={`${m.at}-${String(i)}`} className={m.author === 'user' ? 'msg msg-user' : 'msg msg-agent'}>
              <p className="msg-text">{m.text}</p>
              <time className="msg-time">{m.at.slice(11, 19)}</time>
            </li>
          ))
        )}
      </ol>

      {error !== null && (
        <p className="inline-error" role="alert">
          {error}
        </p>
      )}

      <form
        className="composer"
        onSubmit={(e) => {
          void submit(e);
        }}
      >
        <textarea
          className="composer-input"
          aria-label="消息"
          rows={2}
          value={text}
          placeholder={connected ? '说点什么…（Enter 发送，Shift+Enter 换行）' : '未连接设置中心'}
          disabled={!connected}
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
        <button type="submit" className="composer-send" disabled={!connected || busy || text.trim() === ''}>
          {busy ? '发送中…' : '发送'}
        </button>
      </form>
    </section>
  );
}
