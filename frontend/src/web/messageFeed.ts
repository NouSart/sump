// 消息栏的数据来源。
//
// 结构是**可扩展**的：每个分类登记一个 producer（怎么从实时数据推导卡片），
// 气泡与列表只认"kind + 计数"，不认具体是哪些分类。
// 目前登记了两类：终端消息（命令）与计时消息；终端输出不进消息栏（输出走弹出的终端）。
import type { PortalMessage, TaskView, TimerView } from './types';

export interface FeedInput {
  messages: PortalMessage[];
  timers: TimerView[];
  tasks: TaskView[];
}

/** 每张卡片的公共部分：kind 便于扩展，标题/预览给通用渲染兜底。 */
export interface FeedCardBase {
  kind: string;
  id: string;
  at: string;
  title?: string;
  preview?: string;
  /** 开发模式注入的示例数据（真实数据不会带这个标记）。 */
  mock?: boolean;
}

export interface TerminalCard extends FeedCardBase {
  kind: 'terminal';
  taskId: string;
  taskName: string;
  tool: string;
  command: string;
}

export interface TimerCard extends FeedCardBase {
  kind: 'timer';
  note: string;
  caller: string;
  fireAt: string;
  ms: number;
}

// 已知类别的并集。新增类别时在这里加一个卡片类型即可；
// 气泡与计数只读 kind，排序/图标/文案从 feedKinds 的登记表来。
export type FeedCard = TerminalCard | TimerCard;

type Producer = (input: FeedInput) => FeedCard[];

function messageID(m: PortalMessage): string {
  if (m.id !== undefined && m.id !== '') return m.id;
  if (m.seq !== undefined && m.seq > 0) return `s${String(m.seq)}`;
  return '';
}

function firstLine(text: string): string {
  return text.split('\n').find((line) => line.trim() !== '') ?? '';
}

function terminalProducer({ messages, tasks }: FeedInput): FeedCard[] {
  const taskName = new Map(tasks.map((task) => [task.id, task.name]));
  const cards: FeedCard[] = [];
  for (const m of messages) {
    if (m.kind !== 'tool_call') continue;
    const id = messageID(m);
    if (id === '') continue;
    const taskId = m.taskId ?? '';
    const card: TerminalCard = {
      kind: 'terminal',
      id: `terminal:${id}`,
      at: m.at,
      title: m.meta?.tool ?? '',
      preview: firstLine(m.text),
      taskId,
      taskName: taskName.get(taskId) ?? '',
      tool: m.meta?.tool ?? '',
      command: m.text,
    };
    cards.push(card);
  }
  return cards;
}

function timerProducer({ timers }: FeedInput): FeedCard[] {
  return timers.map((timer) => {
    const start = new Date(timer.fireAt).getTime() - timer.ms;
    const card: TimerCard = {
      kind: 'timer',
      id: `timer:${timer.itemId}`,
      at: new Date(Number.isFinite(start) ? start : Date.now()).toISOString(),
      title: timer.note,
      note: timer.note,
      caller: timer.caller,
      fireAt: timer.fireAt,
      ms: timer.ms,
      mock: timer.mock === true,
    };
    return card;
  });
}

/** 登记表：加一类消息 = 加一个 producer，气泡与列表代码不用动。 */
export const feedProducers: readonly Producer[] = [terminalProducer, timerProducer];

// buildFeed 推导卡片列表：**旧的在前、新的在后**——卡片从下方长出来，向上排。
export function buildFeed(input: FeedInput): FeedCard[] {
  const cards = feedProducers.flatMap((produce) => produce(input));
  return cards.sort((a, b) => timeOf(a.at) - timeOf(b.at));
}

/** 按类别计数：只包含出现过的类别，键由卡片自己的 kind 决定（不写死分类）。 */
export function feedCounts(cards: FeedCard[]): Record<string, number> {
  const counts: Record<string, number> = {};
  for (const card of cards) counts[card.kind] = (counts[card.kind] ?? 0) + 1;
  return counts;
}

function timeOf(at: string): number {
  const value = Date.parse(at);
  return Number.isFinite(value) ? value : 0;
}

// 卡片是否"含换行"：含换行的原文优先上下伸开，而不是先加宽。
export function hasLineBreak(text: string): boolean {
  return text.includes('\n');
}
