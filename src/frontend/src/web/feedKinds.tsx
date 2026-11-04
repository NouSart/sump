// 消息分类的登记表：图标、文案、顺序都在这里。
// 新增一类消息时只加一条登记（再加一个 producer），气泡与列表的代码不动。
import type { ReactElement } from 'react';
import { IconClock, IconSparkle, IconTerminal } from './components/icons';

export interface FeedKindMeta {
  kind: string;
  /** 气泡里用图标，不出现中文文案。 */
  icon: ReactElement;
  /** 读屏与提示用的文案（语言包键 + 中文兜底）。 */
  labelKey: string;
  label: string;
  /** 这类卡片是否带暂停 / 停止控制。 */
  actionable?: boolean;
}

export const FeedKindRegistry: readonly FeedKindMeta[] = [
  {
    kind: 'timer',
    icon: <IconClock />,
    labelKey: 'ui.cards.timer',
    label: '计时',
    actionable: true,
  },
  {
    kind: 'terminal',
    icon: <IconTerminal />,
    labelKey: 'ui.cards.terminal',
    label: '终端',
    actionable: true,
  },
];

const genericKind: FeedKindMeta = {
  kind: '',
  icon: <IconSparkle />,
  labelKey: 'ui.cards.other',
  label: '其他',
};

/** kindMeta 返回某类别的登记；未登记的类别给一个通用图标，不至于渲染不出来。 */
export function kindMeta(kind: string): FeedKindMeta {
  return FeedKindRegistry.find((entry) => entry.kind === kind) ?? { ...genericKind, kind };
}

/** 排序权重：登记表顺序在前，未登记的类别按字母序排在后面。 */
export function kindOrder(kind: string): number {
  const index = FeedKindRegistry.findIndex((entry) => entry.kind === kind);
  return index >= 0 ? index : FeedKindRegistry.length;
}
