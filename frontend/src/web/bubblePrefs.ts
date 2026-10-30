// 消息气泡的本地偏好：新消息到达时只滑气泡、短暂展示多少秒。
// 和外观 / 工作流一样是纯客户端偏好（localStorage），不进服务设置协议。
import { useCallback, useEffect, useState } from 'react';

const PrefKey = 'sump.portal.bubbleSeconds';

/** 默认 4 秒。 */
export const DefaultBubbleSeconds = 4;
export const BubbleSecondsOptions = [2, 4, 6, 8] as const;

export interface BubbleControls {
  seconds: number;
  setSeconds: (seconds: number) => void;
}

function clamp(value: number): number {
  if (!Number.isFinite(value)) return DefaultBubbleSeconds;
  return Math.min(30, Math.max(1, Math.round(value)));
}

function readPref(): number {
  try {
    const saved = Number(window.localStorage.getItem(PrefKey));
    return Number.isFinite(saved) && saved > 0 ? clamp(saved) : DefaultBubbleSeconds;
  } catch {
    return DefaultBubbleSeconds;
  }
}

export function useBubblePrefs(): BubbleControls {
  const [seconds, setSecondsState] = useState(readPref);

  useEffect(() => {
    try {
      window.localStorage.setItem(PrefKey, String(seconds));
    } catch {
      // 隐私模式 / 配额满：记不住就算了，不影响本次使用
    }
  }, [seconds]);

  const setSeconds = useCallback((next: number) => {
    setSecondsState(clamp(next));
  }, []);

  return { seconds, setSeconds };
}
