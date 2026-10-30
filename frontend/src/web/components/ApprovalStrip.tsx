// ApprovalStrip 是门户里的待审区：approval 判不出白名单的命令挂在这里等用户点。
//
// 交互取向：命令原文必须完整可见（用户据此判断），按钮只有「批准执行」与「拒绝」两个，
// 没有"总是允许"——那等于在面板里偷偷改白名单（规则归 approval 的 policy.json）。
import { useState } from 'react';
import type { PendingApprovalView } from '../types';
import type { Translate } from '../i18n';
import { IconCheck, IconClose } from './icons';

interface Props {
  t: Translate;
  items: PendingApprovalView[];
  onDecide: (itemId: string, decision: 'allow' | 'deny') => Promise<void>;
}

export function ApprovalStrip({ items, onDecide, t }: Props) {
  const [busy, setBusy] = useState<string | null>(null);
  if (items.length === 0) return null;
  // expiresMs 是 epoch 毫秒（approval 侧按 TTL 算好）；只到分钟，足够提示"还有多久"。
  const expiryText = (ms: number): string =>
    new Date(ms).toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit' });
  const decide = async (itemId: string, decision: 'allow' | 'deny'): Promise<void> => {
    setBusy(itemId);
    try {
      await onDecide(itemId, decision);
    } finally {
      setBusy(null);
    }
  };
  return (
    <section className="approvals" aria-label={t('ui.approval.title', '待审命令')}>
      <p className="approvals-head">
        {t('ui.approval.title', '待审命令')} · {items.length}
        {t('ui.approval.note', '（命令不会执行，直到你点批准）')}
      </p>
      {items.map((it) => (
        <article key={it.itemId} className="approval" aria-label={`${t('ui.approval.item', '待审')} ${it.itemId}`}>
          <pre className="approval-cmd">{it.command}</pre>
          <p className="approval-meta">
            {it.summary !== '' ? it.summary : t('ui.approval.notCovered', '白名单未覆盖')}
            {it.reasonCode !== '' && <span className="approval-reason"> · {it.reasonCode}</span>}
          </p>
          <p className="approval-meta">
            {it.workDir !== '' && <span>{t('ui.approval.workDir', '目录')} {it.workDir} · </span>}
            {it.requestedBy !== '' && <span>{t('ui.approval.from', '来自')} {it.requestedBy}</span>}
          </p>
          {Number.isFinite(it.expiresMs) && it.expiresMs > 0 && (
            <p className="approval-meta">
              {t('ui.approval.expires', '有效至')} {expiryText(it.expiresMs)}
            </p>
          )}
          <div className="approval-actions">
            <button
              type="button"
              className="btn sm icon"
              aria-label={busy === it.itemId ? t('ui.approval.submitting', '提交中') : t('ui.approval.approve', '批准执行')}
              aria-busy={busy === it.itemId}
              title={t('ui.approval.approve', '批准执行')}
              disabled={busy !== null}
              onClick={() => {
                void decide(it.itemId, 'allow');
              }}
            >
              <IconCheck />
            </button>
            <button
              type="button"
              className="btn sm ghost icon"
              aria-label={t('ui.approval.deny', '拒绝')}
              title={t('ui.approval.deny', '拒绝')}
              disabled={busy !== null}
              onClick={() => {
                void decide(it.itemId, 'deny');
              }}
            >
              <IconClose />
            </button>
          </div>
        </article>
      ))}
    </section>
  );
}
