// 极简 OpenMetrics 注册表：门户只记低基数传输指标（spec §12.3 最小集）。
// 不引第三方库：这里只需要一个 counter / gauge / histogram 的渲染器。

interface Labels {
  [k: string]: string;
}

interface Counter {
  help: string;
  value: number;
}

interface Gauge {
  help: string;
  value: number;
}

interface Histogram {
  help: string;
  unit: string;
  count: number;
  sum: number;
  buckets: number[];
  counts: number[];
}

const counters = new Map<string, Counter>();
const gauges = new Map<string, Gauge>();
const histograms = new Map<string, Histogram>();

const defaultBuckets = [0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10];

// labelKey 只接受调用方写死的小集合；排序保证同一组 label 稳定成一条线。
function labelKey(labels: Labels | undefined): string {
  if (labels === undefined) return '';
  const keys = Object.keys(labels).sort();
  return keys.map((k) => `${k}="${labels[k]}"`).join(',');
}

function sampleName(name: string, labels: Labels | undefined): string {
  const k = labelKey(labels);
  return k === '' ? name : `${name}{${k}}`;
}

export function counter(name: string, help: string, labels: Labels | undefined, delta = 1): void {
  const key = `${name}|${labelKey(labels)}`;
  const cur = counters.get(key);
  counters.set(key, { help, value: (cur?.value ?? 0) + delta });
}

export function gauge(name: string, help: string, labels: Labels | undefined, value: number): void {
  gauges.set(`${name}|${labelKey(labels)}`, { help, value });
}

export function observe(name: string, help: string, unit: string, labels: Labels | undefined, seconds: number): void {
  const key = `${name}|${labelKey(labels)}`;
  let h = histograms.get(key);
  if (h === undefined) {
    h = { help, unit, count: 0, sum: 0, buckets: defaultBuckets, counts: defaultBuckets.map(() => 0) };
    histograms.set(key, h);
  }
  h.count += 1;
  h.sum += seconds;
  for (let i = 0; i < h.buckets.length; i += 1) {
    if (seconds <= h.buckets[i]!) h.counts[i] = (h.counts[i] ?? 0) + 1;
  }
}

// renderOpenMetrics 输出 OpenMetrics 文本（含 +Inf 桶与 # EOF）。
// 同一指标的所有序列必须连续输出（HELP/TYPE 在前），所以先按 name 分组再渲染。
export function renderOpenMetrics(): string {
  const lines: string[] = [];
  const counterNames = new Map<string, [string, Counter][]>();
  for (const [key, c] of counters) {
    const [name, labels] = splitKey(key);
    const list = counterNames.get(name) ?? [];
    list.push([sampleName(name, labels), c]);
    counterNames.set(name, list);
  }
  for (const name of [...counterNames.keys()].sort()) {
    const list = counterNames.get(name)!;
    lines.push(`# HELP ${name} ${list[0]![1].help}`, `# TYPE ${name} counter`);
    for (const [sample, c] of list) lines.push(`${sample} ${c.value}`);
  }

  const gaugeNames = new Map<string, [string, Gauge][]>();
  for (const [key, g] of gauges) {
    const [name, labels] = splitKey(key);
    const list = gaugeNames.get(name) ?? [];
    list.push([sampleName(name, labels), g]);
    gaugeNames.set(name, list);
  }
  for (const name of [...gaugeNames.keys()].sort()) {
    const list = gaugeNames.get(name)!;
    lines.push(`# HELP ${name} ${list[0]![1].help}`, `# TYPE ${name} gauge`);
    for (const [sample, g] of list) lines.push(`${sample} ${g.value}`);
  }

  const histNames = new Map<string, [string, Labels | undefined, Histogram][]>();
  for (const [key, h] of histograms) {
    const [name, labels] = splitKey(key);
    const list = histNames.get(name) ?? [];
    list.push([name, labels, h]);
    histNames.set(name, list);
  }
  for (const name of [...histNames.keys()].sort()) {
    const list = histNames.get(name)!;
    lines.push(`# HELP ${name} ${list[0]![2].help}`, `# TYPE ${name} histogram`);
    for (const [, labels, h] of list) {
      for (let i = 0; i < h.buckets.length; i += 1) {
        lines.push(`${sampleName(`${name}_bucket`, { ...labels, le: String(h.buckets[i]!) })} ${h.counts[i] ?? 0}`);
      }
      lines.push(`${sampleName(`${name}_bucket`, { ...labels, le: '+Inf' })} ${h.count}`);
      lines.push(`${sampleName(`${name}_sum`, labels)} ${h.sum}`);
      lines.push(`${sampleName(`${name}_count`, labels)} ${h.count}`);
    }
  }
  lines.push('# EOF');
  return lines.join('\n') + '\n';
}

function splitKey(key: string): [string, Labels | undefined] {
  const bar = key.indexOf('|');
  const name = key.slice(0, bar);
  const raw = key.slice(bar + 1);
  if (raw === '') return [name, undefined];
  const labels: Labels = {};
  for (const part of raw.split(',')) {
    const eq = part.indexOf('=');
    labels[part.slice(0, eq)] = part.slice(eq + 2, -1);
  }
  return [name, labels];
}

// resetMetricsForTest 清空注册表（单测之间互不影响）。
export function resetMetricsForTest(): void {
  counters.clear();
  gauges.clear();
  histograms.clear();
}