// ApprovalStrip 是门户里的待审区：approval 判不出白名单的命令挂在这里等用户点。
//
// 交互取向：命令原文必须完整可见（用户据此判断），按钮只有「批准执行」与「拒绝」两个，
// 没有"总是允许"——那等于在面板里偷偷改白名单（规则归 approval 的 policy.json）。
import { useState } from 'react';
import type { PendingApprovalView } from '../types';

interface Props {
  items: PendingApprovalView[];
  onDecide: (itemId: string, decision: 'allow' | 'deny') => Promise<void>;
}

export function ApprovalStrip({ items, onDecide }: Props) {
  const [busy, setBusy] = useState<string | null>(null);
  if (items.length === 0) return null;
  const decide = async (itemId: string, decision: 'allow' | 'deny'): Promise<void> => {
    setBusy(itemId);
    try {
      await onDecide(itemId, decision);
    } finally {
      setBusy(null);
    }
  };
  return (
    <section className="approvals" aria-label="待审命令">
      <p className="approvals-head">待审命令 · {items.length}（命令不会执行，直到你点批准）</p>
      {items.map((it) => (
        <article key={it.itemId} className="approval" aria-label={`待审 ${it.itemId}`}>
          <pre className="approval-cmd">{it.command}</pre>
          <p className="approval-meta">
            {it.summary !== '' ? it.summary : '白名单未覆盖'}
            {it.reasonCode !== '' && <span className="approval-reason"> · {it.reasonCode}</span>}
          </p>
          <p className="approval-meta">
            {it.workDir !== '' && <span>目录 {it.workDir} · </span>}
            {it.requestedBy !== '' && <span>来自 {it.requestedBy}</span>}
          </p>
          <div className="approval-actions">
            <button
              type="button"
              className="btn sm"
              disabled={busy !== null}
              onClick={() => {
                void decide(it.itemId, 'allow');
              }}
            >
              {busy === it.itemId ? '提交中…' : '批准执行'}
            </button>
            <button
              type="button"
              className="btn sm ghost"
              disabled={busy !== null}
              onClick={() => {
                void decide(it.itemId, 'deny');
              }}
            >
              拒绝
            </button>
          </div>
        </article>
      ))}
    </section>
  );
}
