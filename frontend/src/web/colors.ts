// 服务身份色：瑞典旗蓝 / 旗黄 / 海报红 / 平涂绿——按服务名哈希分配（名册变动不漂移）。
// 用途：侧栏编号牌、选中填充、eyebrow 方标——颜色即导航（wayfinding）。
const palette = ['#006aa7', '#ffcd00', '#da291c', '#00843d'] as const;

export function serviceColor(name: string): string {
  let h = 0;
  for (const ch of name) h = (h * 31 + (ch.codePointAt(0) ?? 0)) >>> 0;
  return palette[h % palette.length] ?? palette[0];
}

// inkOn 返回在该底色上可读的墨色（亮底给黑、暗底给白）。
export function inkOn(hex: string): string {
  const r = parseInt(hex.slice(1, 3), 16);
  const g = parseInt(hex.slice(3, 5), 16);
  const b = parseInt(hex.slice(5, 7), 16);
  const lum = (0.2126 * r + 0.7152 * g + 0.0722 * b) / 255;
  return lum > 0.6 ? '#111111' : '#ffffff';
}
