// 时间读数的共用格式化：对话的计时条与消息卡片都用同一套写法。
export function remainingText(leftMS: number): string {
  const total = Math.max(0, Math.ceil(leftMS / 1000));
  const two = (n: number): string => String(n).padStart(2, '0');
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  return h > 0 ? `${String(h)}:${two(m)}:${two(s)}` : `${two(m)}:${two(s)}`;
}
