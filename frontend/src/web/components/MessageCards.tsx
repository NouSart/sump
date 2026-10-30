// MessageCards 是浮在对话右侧空白处的一叠消息卡片（手机通知中心的心智模型）：
// 只收**终端消息（命令）**与**计时消息**；新的从下面长出来、向上排；不占布局、不可拖拽。
//
// 交互约定：
//  - 卡片等高、等宽（320），右对齐；一屏放不下就滚，默认保持最新可见；
//  - 多条同时到达时排队，每 200ms 放出一张；
//  - 悬停：未省略略加宽；被省略加宽到完整；到左边界仍不够或原文含换行 → 上下伸开；
//  - 终端消息悬停变成固定大小的「消息模式终端窗口」；
//  - 计时 / 终端卡片悬停显示暂停与停止；暂停后转圈，完成变继续，继续同理；
//  - 可收纳成一枚气泡（分类计数）；手动收起后新消息只滑气泡 N 秒，不强制撑开。
import { useEffect, useRef, useState, type ReactElement } from 'react';
import { IconChevronUp, IconPause, IconPlay, IconStop, IconTerminal } from './icons';
import { hasLineBreak, feedCounts, type FeedCard, type TerminalCard } from '../messageFeed';
import { kindMeta, kindOrder } from '../feedKinds';
import { remainingText } from '../timeText';
import type { Translate } from '../i18n';

/** 卡片基准宽度；等宽指所有卡片同一宽度。 */
export const CardWidth = 320;
/** 卡片基准高度（与样式里的 84px 对应），用于计算"向上展开"要补偿的滚动量。 */
export const CardHeight = 84;
/** 悬停"略加宽"的幅度。 */
const SlightWiden = 24;
/** 终端消息悬停后的固定窗口尺寸。 */
export const TerminalWindowWidth = 480;
export const TerminalWindowHeight = 300;
/** 两条消息出现之间的最小间隔。 */
export const RevealIntervalMS = 200;
/** 暂停 / 继续的模拟耗时（本轮只有 UI）。 */
const ControlDelayMS = 900;
/** 到点后保留展示的时长，之后淡出移除。 */
export const ExpireFadeMS = 4000;
/** 上下边界的渐隐带宽度（与 CSS 对齐）：上边 100px、下边 50px。 */
export const EdgeBandTop = 100;
export const EdgeBandBottom = 50;
/** 贴到边界时效到 0.9；渐隐带之外一律 1.0（正常区域里卡片同尺寸）。 */
const EdgeScale = 0.9;

/** edgeScale 按"卡片进入渐隐带的深度"算缩放：0 = 带内沿（原尺寸），1 = 贴住边界（0.9）。 */
export function edgeScale(depth: number): number {
  const clamped = Math.min(1, Math.max(0, depth));
  return 1 - (1 - EdgeScale) * clamped;
}

type Control = 'idle' | 'pausing' | 'paused' | 'resuming' | 'stopped';

interface Props {
  cards: FeedCard[];
  /** 新消息只滑气泡的秒数。 */
  peekSeconds: number;
  /** 打开设置页时整块向右滑出隐藏，退出后再滑入。 */
  hidden?: boolean;
  t: Translate;
}

function clockText(at: string): string {
  const d = new Date(at);
  if (!Number.isFinite(d.getTime())) return '';
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`;
}

function firstLine(text: string): string {
  return text.split('\n').find((line) => line.trim() !== '') ?? '';
}

// 卡片右缘固定、向左加宽；左边界：左栏展开时是左栏右沿，收起时是屏幕左边界。
function leftLimit(): number {
  const collapsed = document.querySelector('.app[data-collapsed="1"]') !== null;
  if (collapsed) return 0;
  const sidebar = document.querySelector('.sidebar');
  return sidebar instanceof Element ? sidebar.getBoundingClientRect().right : 0;
}

export function MessageCards({ cards, peekSeconds, hidden = false, t }: Props): ReactElement | null {
  // 倒计时：有计时卡片时每秒刷新；没有就不跑定时器。
  const [now, setNow] = useState(() => Date.now());
  const hasTimer = cards.some((card) => card.kind === 'timer');
  useEffect(() => {
    if (!hasTimer) return;
    const id = window.setInterval(() => {
      setNow(Date.now());
    }, 1000);
    return () => {
      window.clearInterval(id);
    };
  }, [hasTimer]);

  const [collapsed, setCollapsed] = useState(false);
  // 窄屏（< 1200px）放不下浮层：只留气泡。
  const [narrow, setNarrow] = useState(false);
  // 逐张放出的进度：记"已经露面的卡片"，旧卡不会因为新卡到来而被挤掉。
  const [shown, setShown] = useState<ReadonlySet<string>>(() => new Set());
  const [peeking, setPeeking] = useState(false);
  const [controls, setControls] = useState<Record<string, Control>>({});
  // 每个计时卡片的暂停账本：累计暂停时长 + 本次暂停起点（暂停期间读数与进度条冻结）。
  const [timing, setTiming] = useState<Record<string, { offset: number; pausedSince?: number }>>({});
  // 到点后先保留 4 秒再淡出移除（timer 快照随后会把它从列表里去掉）。
  const [expiringSince, setExpiringSince] = useState<Record<string, number>>({});
  const [expired, setExpired] = useState<ReadonlySet<string>>(() => new Set());
  const [hover, setHover] = useState<{ id: string; mode: 'slight' | 'wide' | 'tall' | 'terminal'; width: number } | null>(null);
  const listRef = useRef<HTMLOListElement>(null);
  // 展开时把多出来的高度补偿到 scrollTop 上：卡片底边不动，内容向上展开。
  const expandDelta = useRef(0);
  // 是否贴着底部：只在滚动事件里更新。新增卡片会改变 scrollHeight，
  // 若在新增之后再判定，永远判成"不在底部"，就不会跟到最新了。
  const [stick, setStick] = useState(true);
  const measureRefs = useRef(new Map<string, HTMLSpanElement>());
  const prevNewest = useRef('');

  const newest = cards[cards.length - 1];
  const newestID = newest?.id ?? '';

  useEffect(() => {
    if (typeof window.matchMedia !== 'function') return;
    const mq = window.matchMedia('(max-width: 1199px)');
    const apply = (): void => {
      setNarrow(mq.matches);
    };
    apply();
    mq.addEventListener('change', apply);
    return () => {
      mq.removeEventListener('change', apply);
    };
  }, []);

  const collapsedView = collapsed || narrow;

  useEffect(() => {
    const ids = cards.map((card) => card.id);
    if (ids.length === 0) {
      // 空列表时只在"确实有已露面的卡片"才清空；否则每次返回新 Set 会造成无限重渲染。
      setShown((prev) => (prev.size === 0 ? prev : new Set()));
      return;
    }
    const known = new Set(ids);
    // 卡片消失（例如计时到点）后把记录收回来。
    setShown((prev) => {
      const next = new Set([...prev].filter((id) => known.has(id)));
      return next.size === prev.size ? prev : next;
    });
    // 还没露面的按时间顺序逐张放出：首张立即出现，之后每 200ms 一张。
    const pending = ids.find((id) => !shown.has(id));
    if (pending === undefined) return;
    if (shown.size === 0) {
      setShown(new Set([pending]));
      return;
    }
    const timer = window.setTimeout(() => {
      setShown((prev) => new Set([...prev, pending]));
    }, RevealIntervalMS);
    return () => {
      window.clearTimeout(timer);
    };
  }, [cards.map((card) => card.id).join('|'), shown]);

  // 新的最上面一张出现时：若已收起，只滑气泡 N 秒；手动收起本身不触发。
  useEffect(() => {
    const changed = prevNewest.current !== '' && prevNewest.current !== newestID;
    prevNewest.current = newestID;
    if (!collapsedView || !changed) return;
    setPeeking(true);
    const timer = window.setTimeout(() => {
      setPeeking(false);
    }, peekSeconds * 1000);
    return () => {
      window.clearTimeout(timer);
    };
  }, [newestID, collapsedView, peekSeconds]);

  // 保持最新可见：新的从下面长出来、旧的向上挤压；贴底时自动跟到底，往上翻看旧卡片时不抢。
  // 从气泡展开时也要落到最新一张（挂载瞬间 scrollHeight 还没定，补一帧再滚一次）。
  useEffect(() => {
    const list = listRef.current;
    if (list === null || !stick) return;
    list.scrollTop = list.scrollHeight;
    const frame = window.requestAnimationFrame(() => {
      list.scrollTop = list.scrollHeight;
    });
    return () => {
      window.cancelAnimationFrame(frame);
    };
  }, [shown, cards.length, stick, collapsedView]);

  const counts = feedCounts(cards);

  const control = (id: string): Control => controls[id] ?? 'idle';
  // effectiveNow：暂停期间冻结在暂停时刻（扣掉此前累计的暂停时长）。
  const effectiveNow = (id: string): number => {
    const entry = timing[id];
    const base = now - (entry?.offset ?? 0);
    if (entry?.pausedSince === undefined) return base;
    return entry.pausedSince - entry.offset;
  };
  const runControl = (id: string, next: Control, spin: Control): void => {
    setControls((prev) => ({ ...prev, [id]: spin }));
    window.setTimeout(() => {
      setControls((prev) => ({ ...prev, [id]: next }));
      setTiming((prev) => {
        const entry = prev[id] ?? { offset: 0 };
        if (next === 'paused') return { ...prev, [id]: { ...entry, pausedSince: Date.now() } };
        if (next === 'idle' && entry.pausedSince !== undefined) {
          return { ...prev, [id]: { offset: entry.offset + (Date.now() - entry.pausedSince) } };
        }
        return prev;
      });
    }, ControlDelayMS);
  };

  // 悬停展开要"向上长"：卡片底边保持原位，多出来的高度用 scrollTop 抵消。
  useEffect(() => {
    const list = listRef.current;
    if (list === null) return;
    const prev = expandDelta.current;
    if (hover === null) {
      if (prev > 0) {
        list.scrollTop = Math.max(0, list.scrollTop - prev);
        expandDelta.current = 0;
      }
      return;
    }
    const el = Array.from(list.querySelectorAll<HTMLElement>('[data-card]')).find(
      (node) => node.dataset.card === hover.id,
    );
    if (el === undefined) return;
    const delta = el.getBoundingClientRect().height - CardHeight;
    if (delta <= 0) return;
    list.scrollTop += delta - prev;
    expandDelta.current = delta;
  }, [hover]);

  // 边界缩放：卡片进入上边界或下边界的 50px 渐隐带时都会缩小（两边对称）。
  // 这一层写在卡片本体上，与内联在 <li> 上的阶梯缩放叠加。
  useEffect(() => {
    const list = listRef.current;
    if (list === null) return;
    let frame = 0;
    const apply = (): void => {
      frame = 0;
      const listRect = list.getBoundingClientRect();
      for (const el of list.querySelectorAll<HTMLElement>('.mcard')) {
        const item = el.parentElement ?? el;
        const rect = item.getBoundingClientRect();
        const depthTop = (listRect.top + EdgeBandTop - rect.top) / EdgeBandTop;
        const depthBottom = (rect.bottom - (listRect.bottom - EdgeBandBottom)) / EdgeBandBottom;
        const depth = Math.max(depthTop, depthBottom);
        el.style.transform = `scale(${edgeScale(depth).toFixed(3)})`;
        el.style.transformOrigin = 'bottom right';
      }
    };
    const schedule = (): void => {
      if (frame !== 0) return;
      frame = window.requestAnimationFrame(apply);
    };
    // 首次与"展开回来"都先立刻算一遍，再补一帧等布局落定（收纳后重建卡片时尤其需要）。
    apply();
    const first = window.requestAnimationFrame(apply);
    list.addEventListener('scroll', schedule, { passive: true });
    return () => {
      list.removeEventListener('scroll', schedule);
      window.cancelAnimationFrame(first);
      if (frame !== 0) window.cancelAnimationFrame(frame);
    };
  }, [shown, hover, cards.length, collapsedView]);

  // Esc 收起消息区（设置页打开时不抢这个键）。
  useEffect(() => {
    if (collapsedView) return;
    const onKey = (e: KeyboardEvent): void => {
      if (e.key !== 'Escape') return;
      if (document.querySelector('.settings') !== null) return;
      setCollapsed(true);
    };
    window.addEventListener('keydown', onKey);
    return () => {
      window.removeEventListener('keydown', onKey);
    };
  }, [collapsedView]);

  // 到点：计一次开始时间，保留 ExpireFadeMS 之后从列表移除。
  useEffect(() => {
    if (!hasTimer) return;
    const started: string[] = [];
    for (const card of cards) {
      if (card.kind !== 'timer' || expiringSince[card.id] !== undefined) continue;
      if (effectiveNow(card.id) >= new Date(card.fireAt).getTime()) started.push(card.id);
    }
    if (started.length === 0) return;
    setExpiringSince((prev) => {
      const next = { ...prev };
      for (const id of started) next[id] = Date.now();
      return next;
    });
  }, [now, cards, expiringSince, timing]);

  useEffect(() => {
    const pending = Object.keys(expiringSince).filter((id) => !expired.has(id));
    if (pending.length === 0) return;
    const timers = pending.map((id) =>
      window.setTimeout(() => {
        setExpired((prev) => (prev.has(id) ? prev : new Set([...prev, id])));
      }, ExpireFadeMS),
    );
    return () => {
      for (const timer of timers) window.clearTimeout(timer);
    };
  }, [expiringSince, expired]);

  const enter = (card: FeedCard, el: HTMLElement): void => {
    if (card.kind === 'terminal') {
      setHover({ id: card.id, mode: 'terminal', width: TerminalWindowWidth });
      return;
    }
    const text = card.title !== undefined && card.title !== '' ? card.title : (card.preview ?? '');
    if (hasLineBreak(text)) {
      setHover({ id: card.id, mode: 'tall', width: CardWidth });
      return;
    }
    const measured = measureRefs.current.get(card.id)?.scrollWidth ?? 0;
    const needed = measured > 0 ? measured + 64 : CardWidth;
    if (needed <= CardWidth) {
      setHover({ id: card.id, mode: 'slight', width: CardWidth + SlightWiden });
      return;
    }
    const cardRight = el.getBoundingClientRect().right;
    const boundary = cardRight - leftLimit() - 12;
    // 布局未就绪（jsdom 里所有矩形都是 0）时不做边界判断，按"加宽"处理。
    if (cardRight <= 0) {
      setHover({ id: card.id, mode: 'wide', width: Math.min(needed, CardWidth * 2) });
      return;
    }
    if (needed <= boundary) setHover({ id: card.id, mode: 'wide', width: needed });
    else setHover({ id: card.id, mode: 'tall', width: CardWidth });
  };

  const sizeOf = (id: string): { width: number; tall: boolean; terminal: boolean } => {
    if (hover === null || hover.id !== id) return { width: CardWidth, tall: false, terminal: false };
    return { width: hover.width, tall: hover.mode === 'tall', terminal: hover.mode === 'terminal' };
  };

  const visible = cards.filter((card) => shown.has(card.id) && !expired.has(card.id));
  // 只显示"有数量"的类别；一个都没有就整块隐藏（组件顶部已对空列表返回 null）。
  const kindEntries = Object.entries(counts)
    .filter(([, count]) => count > 0)
    .sort(([a], [b]) => {
      const byRegistry = kindOrder(a) - kindOrder(b);
      return byRegistry !== 0 ? byRegistry : a.localeCompare(b);
    });
  if (kindEntries.length === 0) return null;
  const kindLabel = (kind: string): string => {
    const meta = kindMeta(kind);
    return t(meta.labelKey, meta.label);
  };
  const summary = kindEntries.map(([kind, count]) => `${kindLabel(kind)} ${String(count)}`).join(' · ');

  return (
    <div
      className={[collapsedView ? 'mcards is-collapsed' : 'mcards', hidden ? 'is-hidden' : ''].filter((c) => c !== '').join(' ')}
      aria-hidden={hidden}
    >
      {collapsedView ? (
        <button
          type="button"
          className={peeking ? 'bubble mbubble is-peek' : 'bubble mbubble'}
          onClick={() => {
            // 展开即落到最新一张：先把"贴底"打开，再展开。
            setStick(true);
            setCollapsed(false);
            setPeeking(false);
          }}
          aria-label={`${t('ui.cards.expand', '展开消息')}：${summary}`}
          title={summary}
        >
          {kindEntries.map(([kind, count]) => (
            <span key={kind} className="mbubble-kind">
              <span className="mbubble-glyph" aria-hidden="true">
                {kindMeta(kind).icon}
              </span>
              <span className="mbubble-count">{count}</span>
            </span>
          ))}
          {peeking && newest !== undefined && (
            <span className="mbubble-peek">{newest.kind === 'terminal' ? firstLine((newest as TerminalCard).command) : (newest.title ?? '')}</span>
          )}
        </button>
      ) : (
        <>
          <ol
            className="mcards-list"
            ref={listRef}
            onDoubleClick={() => {
              setCollapsed(true);
            }}
            onScroll={(e) => {
              const el = e.currentTarget;
              const next = el.scrollHeight - el.scrollTop - el.clientHeight <= 24;
              setStick((prev) => (prev === next ? prev : next));
            }}
          >
            {visible.map((card) => {
              const size = sizeOf(card.id);
              const state = control(card.id);
              const meta = kindMeta(card.kind);
              const actionable = meta.actionable === true;
              const title = card.title !== undefined && card.title !== '' ? card.title : kindLabel(card.kind);
              const preview = card.kind === 'timer' ? '' : (card.preview ?? '');
              // 计时读数与进度：以 fireAt 为准，暂停时冻结在暂停时刻。
              const startAt = new Date(card.at).getTime();
              const fireAt = card.kind === 'timer' ? new Date(card.fireAt).getTime() : 0;
              const eff = card.kind === 'timer' ? effectiveNow(card.id) : now;
              const total = Math.max(1, fireAt - startAt);
              const elapsed = Math.min(total, Math.max(0, eff - startAt));
              const percent = card.kind === 'timer' ? Math.round((elapsed / total) * 100) : 0;
              const left = card.kind === 'timer' ? Math.max(0, fireAt - eff) : 0;
              const cls = [
                'mcard',
                `mcard-${card.kind}`,
                size.tall ? 'is-tall' : '',
                size.terminal ? 'is-terminal' : '',
                state === 'stopped' ? 'is-stopped' : '',
                state === 'paused' ? 'is-paused' : '',
                card.kind === 'timer' && left <= 0 ? 'is-expired' : '',
              ]
                .filter((name) => name !== '')
                .join(' ');
              return (
                <li key={card.id} className="mcard-item">
                  <article
                    className={cls}
                    style={{ width: `${String(size.width)}px` }}
                    tabIndex={0}
                    onClick={(e) => {
                      // 点一下展开；再点一次或鼠标移出对应卡片即收回。
                      if (hover !== null && hover.id === card.id) setHover(null);
                      else enter(card, e.currentTarget);
                    }}
                    onKeyDown={(e) => {
                      if (e.key !== 'Enter' && e.key !== ' ') return;
                      e.preventDefault();
                      if (hover !== null && hover.id === card.id) setHover(null);
                      else enter(card, e.currentTarget);
                    }}
                    onMouseLeave={() => {
                      setHover((prev) => (prev !== null && prev.id === card.id ? null : prev));
                    }}
                    data-card={card.id}
                  >
                    <div className="mcard-head">
                      <span className="mcard-glyph" aria-hidden="true">
                        {meta.icon}
                      </span>
                      <span className="mcard-title">{title}</span>
                      {card.mock === true && <span className="mcard-mock">{t('ui.cards.mock', 'mock')}</span>}
                      <time className="mcard-time" dateTime={card.at}>
                        {clockText(card.at)}
                      </time>
                    </div>

                    {size.terminal ? (
                      <div className="mterm" style={{ height: `${String(TerminalWindowHeight - 64)}px` }}>
                        <p className="mterm-line">
                          <span className="mterm-prompt" aria-hidden="true">
                            $
                          </span>
                          {card.kind === 'terminal' ? (card as TerminalCard).command : ''}
                        </p>
                        <p className="mterm-hint">{t('ui.cards.outputHint', '终端输出不在此列；点开终端查看。')}</p>
                      </div>
                    ) : card.kind === 'timer' ? (
                      <>
                        <p className="mcard-body">
                          {left <= 0
                            ? t('ui.portal.timerDue', '到点…')
                            : `${remainingText(left)} ${t('ui.portal.timerWake', '后唤醒')}`}
                        </p>
                        <div
                          className="mcard-progress"
                          role="progressbar"
                          aria-valuemin={0}
                          aria-valuemax={100}
                          aria-valuenow={percent}
                          aria-label={title}
                        >
                          <span style={{ width: `${String(percent)}%` }} />
                        </div>
                      </>
                    ) : (
                      <p className="mcard-body">{preview}</p>
                    )}

                    {actionable && (
                      <div className="mcard-actions">
                        {state === 'stopped' ? (
                          <span className="mcard-stopped">{t('ui.cards.stopped', '已停止')}</span>
                        ) : (
                          <>
                            <button
                              type="button"
                              className="mcard-action"
                              aria-busy={state === 'pausing' || state === 'resuming'}
                              aria-label={
                                state === 'paused'
                                  ? t('ui.cards.resume', '继续')
                                  : t('ui.cards.pause', '暂停')
                              }
                              title={state === 'paused' ? t('ui.cards.resume', '继续') : t('ui.cards.pause', '暂停')}
                              disabled={state === 'pausing' || state === 'resuming'}
                              onClick={() => {
                                if (state === 'paused') runControl(card.id, 'idle', 'resuming');
                                else runControl(card.id, 'paused', 'pausing');
                              }}
                            >
                              {state === 'pausing' || state === 'resuming' ? (
                                <span className="mcard-spinner" aria-hidden="true" />
                              ) : state === 'paused' ? (
                                <IconPlay />
                              ) : (
                                <IconPause />
                              )}
                            </button>
                            <button
                              type="button"
                              className="mcard-action"
                              aria-label={t('ui.cards.stop', '停止')}
                              title={t('ui.cards.stop', '停止')}
                              onClick={() => {
                                setControls((prev) => ({ ...prev, [card.id]: 'stopped' }));
                              }}
                            >
                              <IconStop />
                            </button>
                          </>
                        )}
                      </div>
                    )}

                    <span
                      className="mcard-measure"
                      aria-hidden="true"
                      ref={(el) => {
                        if (el === null) measureRefs.current.delete(card.id);
                        else measureRefs.current.set(card.id, el);
                      }}
                    >
                      {card.kind === 'terminal' ? firstLine(card.command) : title}
                    </span>
                  </article>
                </li>
              );
            })}
          </ol>
          {/* 右下角的收起按钮：抬头已去掉，收纳入口留在这里（Esc / 双击卡片区同样可收起）。 */}
          <button
            type="button"
            className="mcards-collapse-bottom"
            onClick={() => {
              setCollapsed(true);
            }}
            aria-label={t('ui.cards.collapse', '收纳消息')}
            title={t('ui.cards.collapse', '收纳消息')}
          >
            <IconChevronUp />
          </button>
        </>
      )}
    </div>
  );
}
