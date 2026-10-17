import { useEffect, useRef, useState } from 'react';
import type { PendingApprovalView, PortalMessage, TimerView } from '../types';
import { ApprovalStrip } from './ApprovalStrip';

interface Props {
  connected: boolean;
  messages: PortalMessage[];
  timers: TimerView[];
  approvals: PendingApprovalView[];
  onDecide: (itemId: string, decision: 'allow' | 'deny') => Promise<void>;
  error: string | null;
  onSend: (text: string) => Promise<void>;
  onOpenSettings: () => void;
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

// remainingText 把剩余毫秒写成人读的读数：mm:ss，超过一小时才带小时。
export function remainingText(leftMS: number): string {
  const total = Math.max(0, Math.ceil(leftMS / 1000));
  const two = (n: number): string => String(n).padStart(2, '0');
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return h > 0 ? `${String(h)}:${two(m)}:${two(s)}` : `${two(m)}:${two(s)}`;
}

// PortalView 是门户的对话面：上方消息流，下方输入行。
// 消息只有两条来源——本地回显的输入，与 SSE 送来的 deliver。
export function PortalView({ connected, messages, timers, approvals, onDecide, error, onSend, onOpenSettings }: Props) {
  const [text, setText] = useState('');
  const [busy, setBusy] = useState(false);
  const listRef = useRef<HTMLOListElement>(null);
  const now = useTick(timers.length > 0);

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
    <section className="portal" aria-label="对话">
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

      {/* 待审命令：白名单没覆盖的命令挂起在这里，等用户点批准或拒绝。
          放在输入框正上方——它是"需要你现在做决定"的东西，不能滚出视野。 */}
      <ApprovalStrip items={approvals} onDecide={onDecide} />

      {/* 在计时器：timer 推来全量快照，这里只负责画读数。
          到点后不清屏——timer 会推一条不含它的快照过来；快照还没到就显示「到点…」，
          绝不假装"没有计时"。 */}
      {timers.length > 0 && (
        <ol className="timer-strip" aria-label="在计时器">
          {timers.map((t) => {
            const left = new Date(t.fireAt).getTime() - now;
            return (
              <li key={t.itemId} className="timer-item">
                <span className="timer-left">{left <= 0 ? '到点…' : `${remainingText(left)} 后唤醒`}</span>
                {t.note !== '' && <span className="timer-note">{t.note}</span>}
              </li>
            );
          })}
        </ol>
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
        <div className="composer-row">
          <button type="button" className="chip" onClick={onOpenSettings}>
            设置
          </button>
          <button type="button" className="chip" disabled title="附件上传（下一步）">
            添加附件
          </button>
          <button type="button" className="chip" disabled title="终端面板（下一步）">
            终端
          </button>
          <span className="grow" />
          <button type="submit" className="chip primary" disabled={!connected || busy || text.trim() === ''}>
            {busy ? '发送中…' : '发送'}
          </button>
        </div>
      </form>
    </section>
  );
}
