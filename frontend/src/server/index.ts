// frontend：SUMP v2 的用户交互门户（与 qq 同类）＋ 设置中心可视化面板。
// 服务节点：把自己注册进设置中心（心跳 / 名册）；浏览器侧：HTTP JSON + SSE。
// 用法：node dist/server/index.js [-addr 127.0.0.1:9501] [-center ws://127.0.0.1:9000/ws] [-name frontend]
//        [-web dist/web] [-heartbeat-ms 15000] [-reasoner reasoner] [-memory memory]
//        [-files files] [-images images] [-shell shell] [-upload-max-bytes 20971520]
//        [-locales locales] [-lang zh-CN] [-trusted-hosts a.example,b.example]
import { dirname, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { startFrontend, type FrontendConfig } from './app.ts';

// parseFlags 解析 Go 风格命令行：-flag value / --flag=value（与其余服务一致）。
function parseFlags(argv: string[]): Record<string, string> {
  const out: Record<string, string> = {};
  for (let i = 0; i < argv.length; i++) {
    const m = /^--?([A-Za-z][\w-]*)(?:=(.*))?$/.exec(argv[i] ?? '');
    if (!m) continue;
    const key = m[1] ?? '';
    if (m[2] !== undefined) {
      out[key] = m[2];
      continue;
    }
    const next = argv[i + 1];
    if (next !== undefined && !next.startsWith('-')) {
      out[key] = next;
      i++;
    } else {
      out[key] = 'true';
    }
  }
  return out;
}

// defaultLocalesDir：语言包在仓库根的 locales/（dist/server → 上三级；也允许 -locales 覆盖）。
function defaultLocalesDir(): string {
  const here = dirname(fileURLToPath(import.meta.url)); // frontend/dist/server
  return resolve(here, '../../../locales');
}

function resolveFlags(argv: string[]): FrontendConfig {
  const raw = parseFlags(argv);
  const heartbeatMS = raw['heartbeat-ms'] === undefined ? 15_000 : Number(raw['heartbeat-ms']);
  if (!Number.isFinite(heartbeatMS) || heartbeatMS <= 0) {
    throw new Error(`非法 -heartbeat-ms: ${raw['heartbeat-ms'] ?? ''}`);
  }
  const here = dirname(fileURLToPath(import.meta.url)); // dist/server
  const uploadMaxBytes = raw['upload-max-bytes'] === undefined ? 20 << 20 : Number(raw['upload-max-bytes']);
  if (!Number.isFinite(uploadMaxBytes) || uploadMaxBytes <= 0) {
    throw new Error(`非法 -upload-max-bytes: ${raw['upload-max-bytes'] ?? ''}`);
  }
  return {
    addr: raw.addr ?? '127.0.0.1:9501',
    centerURL: raw.center ?? 'ws://127.0.0.1:9000/ws',
    name: raw.name ?? 'frontend',
    webDir: raw.web ?? resolve(here, '../web'),
    heartbeatMS,
    reasoner: raw.reasoner ?? 'reasoner',
    memory: raw.memory ?? 'memory',
    localesDir: raw.locales ?? defaultLocalesDir(),
    lang: raw.lang ?? 'zh-CN',
    approval: raw.approval ?? 'approval',
    files: raw.files ?? 'files',
    images: raw.images ?? 'images',
    shell: raw.shell ?? 'shell',
    uploadMaxBytes,
    trustedHosts: (raw['trusted-hosts'] ?? '')
      .split(',')
      .map((h) => h.trim())
      .filter((h) => h !== ''),
  };
}

const log = (msg: string): void => {
  console.log(`[frontend] ${new Date().toISOString()} ${msg}`);
};

const flags = resolveFlags(process.argv.slice(2));
const handle = await startFrontend(flags, log);
log(`面板入口 http://${handle.addr}/（静态目录 ${flags.webDir}）`);
log(`服务节点 ${flags.name}：ws://${handle.addr}/ws → 注册到 ${flags.centerURL}`);

let stopping = false;
const shutdown = (): void => {
  if (stopping) return;
  stopping = true;
  log('退出');
  void handle.close().then(() => process.exit(0));
};
process.on('SIGINT', shutdown);
process.on('SIGTERM', shutdown);
