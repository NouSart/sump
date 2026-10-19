#!/usr/bin/env node
/**
 * 前端构建指纹（build stamp）。
 *
 * 为什么要有它：mtime 只能回答「dist 是不是比 src 旧」，回答不了「上次构建之后 src 到底动没动过、
 * 动了哪几个」。时钟被改、git checkout 把文件 mtime 刷成当前时间，mtime 都会说谎。
 * 这个脚本在 npm run build 成功后，把「参与构建的每个文件的内容指纹（sha1）+ mtime」写进
 * frontend/dist/.build-stamp；之后随时用 check 只读比对，就能准确回答「动没动过」。
 *
 * 用法（在 frontend/ 下）：
 *   node scripts/build-stamp.mjs write     写指纹（npm run build 成功后已自动调用）
 *   node scripts/build-stamp.mjs check     只读体检：动没动过、要不要重建
 *   node scripts/build-stamp.mjs check -v  同上，并列出每个文件的比对结果
 *   node scripts/build-stamp.mjs adopt     没有指纹（老 dist）时，按当前 dist 建立基线
 *
 * check 退出码：0=无需重建（含"无指纹但 mtime 判定最新"）  10=需要重建
 *              2=无法判定（出错，按需要重建处理）
 */

import { createHash } from 'node:crypto';
import { existsSync } from 'node:fs';
import { readFile, readdir, stat, writeFile, mkdir } from 'node:fs/promises';
import path from 'node:path';
import { fileURLToPath } from 'node:url';

const here = path.dirname(fileURLToPath(import.meta.url));
const frontendDir = path.resolve(here, '..');
const distDir = path.join(frontendDir, 'dist');
const stampPath = path.join(distDir, '.build-stamp');
const distWebIndex = path.join(distDir, 'web', 'index.html');
const distServerIndex = path.join(distDir, 'server', 'index.js');

// 参与构建的输入 = src 下所有文件 + 这些构建/类型检查配置。
// package-lock.json 故意不算：npm install 会刷它，但依赖没变时重建没意义；依赖真变了 package.json 也会变。
const CONFIG_FILES = [
  'package.json',
  'index.html',
  'vite.config.ts',
  'vitest.config.ts',
  'tsconfig.json',
  'tsconfig.web.json',
  'tsconfig.test.json',
];

const EXIT_CLEAN = 0;
const EXIT_DIRTY = 10;
const EXIT_NO_STAMP = 11;
const EXIT_UNKNOWN = 2;
function pad2(n) {
  return String(n).padStart(2, '0');
}

function fmtLocal(d) {
  const y = d.getFullYear();
  return `${y}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}:${pad2(d.getSeconds())}`;
}

function fmtLocalShort(d) {
  return `${pad2(d.getMonth() + 1)}-${pad2(d.getDate())} ${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

function sha1(buf) {
  return createHash('sha1').update(buf).digest('hex');
}

function relOf(abs) {
  return path.relative(frontendDir, abs).split(path.sep).join('/');
}

async function listFiles(dir) {
  if (!existsSync(dir)) return [];
  const entries = await readdir(dir, { recursive: true, withFileTypes: true });
  const out = [];
  for (const e of entries) {
    if (!e.isFile()) continue;
    const parent = e.parentPath || e.path;
    out.push(path.join(parent, e.name));
  }
  return out;
}
/** 扫描当前工作区里「会进构建的所有输入」，返回逐文件指纹。 */
async function collect() {
  const rels = new Set();
  for (const abs of await listFiles(path.join(frontendDir, 'src'))) rels.add(relOf(abs));
  for (const name of CONFIG_FILES) {
    if (existsSync(path.join(frontendDir, name))) rels.add(name);
  }

  const sorted = [...rels].sort();
  const files = {};
  let maxMtimeMs = 0;
  let maxMtimeRel = '';

  for (const rel of sorted) {
    const abs = path.join(frontendDir, rel);
    const [buf, st] = await Promise.all([readFile(abs), stat(abs)]);
    files[rel] = { sha1: sha1(buf), mtime: new Date(st.mtimeMs).toISOString(), size: st.size };
    if (st.mtimeMs > maxMtimeMs) {
      maxMtimeMs = st.mtimeMs;
      maxMtimeRel = rel;
    }
  }

  const contentHash = sha1(sorted.map((r) => `${r}\n${files[r].sha1}`).join('\n'));
  return { sorted, files, maxMtimeMs, maxMtimeRel, contentHash };
}

async function readStamp() {
  try {
    const s = JSON.parse(await readFile(stampPath, 'utf8'));
    if (!s || typeof s !== 'object' || !s.files || typeof s.files !== 'object') return null;
    return s;
  } catch {
    return null;
  }
}

async function writeStamp(builtAtMs, opts) {
  const cur = await collect();
  const builtAt = new Date(builtAtMs);
  const stamp = {
    version: 1,
    tool: 'frontend/scripts/build-stamp.mjs',
    builtAt: builtAt.toISOString(),
    builtAtLocal: fmtLocal(builtAt),
    adopted: Boolean(opts && opts.adopted),
    fileCount: cur.sorted.length,
    maxMtime: new Date(cur.maxMtimeMs).toISOString(),
    maxMtimeLocal: fmtLocal(new Date(cur.maxMtimeMs)),
    maxMtimeFile: cur.maxMtimeRel,
    contentHash: cur.contentHash,
    files: cur.files,
  };
  await mkdir(distDir, { recursive: true });
  await writeFile(stampPath, JSON.stringify(stamp, null, 2) + '\n', 'utf8');
  return stamp;
}

async function cmdWrite() {
  const stamp = await writeStamp(Date.now(), { adopted: false });
  console.log(`已写入构建指纹：${relOf(stampPath)}（${stamp.fileCount} 个源文件，指纹 ${stamp.contentHash.slice(0, 8)}）`);
  return EXIT_CLEAN;
}

async function cmdAdopt() {
  if (!existsSync(distWebIndex)) {
    console.log('✗ 没有 dist/web/index.html，无法建立基线——请先 npm run build。');
    return EXIT_UNKNOWN;
  }
  const distMs = (await stat(distWebIndex)).mtimeMs;
  const stamp = await writeStamp(distMs, { adopted: true });
  console.log(`已按当前 dist 建立基线：${relOf(stampPath)}（构建时间取 dist/web/index.html 的 mtime = ${stamp.builtAtLocal}，${stamp.fileCount} 个源文件）`);
  console.log('  注意：基线只是记录"现在这个 dist 对应这套源码"，之后才能真正回答"动没动过"。');
  return EXIT_CLEAN;
}

function indent(line, n) {
  return ' '.repeat(n === undefined ? 2 : n) + line;
}

async function cmdCheck(argv) {
  const verbose = argv.includes('-v') || argv.includes('--verbose');
  const say = (s) => console.log(s === undefined ? '' : s);

  say('前端体检（只读：不构建、不重启、不改任何文件）');
  say(indent('目录：' + frontendDir));

  const distOk = existsSync(distWebIndex) && existsSync(distServerIndex);
  if (distOk) {
    const w = await stat(distWebIndex);
    const s = await stat(distServerIndex);
    say(indent('dist：web/index.html ' + fmtLocal(new Date(w.mtimeMs)) + ' / server/index.js ' + fmtLocal(new Date(s.mtimeMs))));
  } else {
    say(indent('dist：缺失（dist/web/index.html 或 dist/server/index.js 不存在）'));
  }

  const cur = await collect();
  say(indent('源码：' + cur.sorted.length + ' 个输入文件，最新 ' + cur.maxMtimeRel + '（' + fmtLocal(new Date(cur.maxMtimeMs)) + '）'));

  if (!distOk) {
    say('');
    say('结论：✗ 需要重建 —— dist 缺失，没有可用的 bundle');
    say(indent('下一步：cd frontend; npm run build', 7));
    return EXIT_DIRTY;
  }

  const stamp = await readStamp();
  if (!stamp) {
    const distMs = (await stat(distWebIndex)).mtimeMs;
    if (cur.maxMtimeMs > distMs) {
      const delta = Math.round((cur.maxMtimeMs - distMs) / 1000);
      say('');
      say('结论：✗ 需要重建 —— 没有构建指纹，只能看 mtime：源码比 dist 新');
      say(indent('最新源：' + cur.maxMtimeRel + '（' + fmtLocal(new Date(cur.maxMtimeMs)) + '），比 dist 新 ' + delta + ' 秒', 7));
      say(indent('下一步：cd frontend; npm run build（成功后自动写指纹，从此不再靠 mtime 猜）', 7));
      return EXIT_DIRTY;
    }
    say('');
    say('结论：~ 无指纹，按 mtime 判定为最新 —— dist 比源码新（老办法，只能看时间戳）');
    say(indent('建立基线：node frontend/scripts/build-stamp.mjs adopt（之后就能准确回答"动没动过"）', 7));
    return EXIT_CLEAN;
  }

  const added = [];
  const removed = [];
  const changed = [];
  const touched = [];
  for (const rel of cur.sorted) {
    const old = stamp.files[rel];
    if (!old) added.push(rel);
    else if (old.sha1 !== cur.files[rel].sha1) changed.push(rel);
    else if (old.mtime !== cur.files[rel].mtime) touched.push(rel);
  }
  for (const rel of Object.keys(stamp.files)) {
    if (!cur.files[rel]) removed.push(rel);
  }
  added.sort();
  removed.sort();
  changed.sort();
  touched.sort();

  const how = stamp.adopted ? '（按 dist 建的基线，非真实构建时刻）' : '';
  say(indent('指纹：上次构建 ' + stamp.builtAtLocal + how + '，记录 ' + stamp.fileCount + ' 个源文件，指纹 ' + String(stamp.contentHash).slice(0, 8)));

  const dirty = added.length + removed.length + changed.length > 0;
  const LIMIT = verbose ? 1e9 : 8;
  const showList = (label, list, extra) => {
    if (list.length === 0) return;
    say(indent(label + ' ' + list.length + ' 个：', 2));
    for (const rel of list.slice(0, LIMIT)) {
      const t = cur.files[rel] ? '（' + fmtLocalShort(new Date(cur.files[rel].mtime)) + '）' : '';
      say(indent('- ' + rel + t + (extra || ''), 4));
    }
    if (list.length > LIMIT) say(indent('……还有 ' + (list.length - LIMIT) + ' 个（加 -v 看全部）', 4));
  };

  say('');
  if (!dirty) {
    say('结论：✓ 无改动 —— 上次构建之后前端一个文件都没动，当前 bundle（' + fmtLocalShort(new Date(stamp.builtAt)) + ' 构建）就是它');
    if (touched.length > 0) {
      say(indent('注：' + touched.length + ' 个文件 mtime 变了但内容没变（git checkout / 解压常见），不算改动', 7));
      if (verbose) showList('mtime 变动但内容相同', touched);
    }
    return EXIT_CLEAN;
  }

  say('结论：✗ 有改动，需要重建 —— 上次构建（' + stamp.builtAtLocal + '）之后有 ' + (changed.length + added.length + removed.length) + ' 个文件变了');
  showList('修改', changed, '（内容变了）');
  showList('新增', added, '（上次构建时不存在的输入）');
  showList('删除', removed, '（上次构建时存在、现在没了）');
  if (touched.length > 0) {
    say(indent('另：' + touched.length + ' 个文件 mtime 变了但内容没变，不算改动', 2));
    if (verbose) showList('mtime 变动但内容相同', touched);
  }
  say(indent('下一步：cd frontend; npm run build（或 scripts/reload-sump.ps1，会自动重建）', 2));
  return EXIT_DIRTY;
}

async function main() {
  const cmd = (process.argv[2] || 'check').toLowerCase();
  const rest = process.argv.slice(3);
  if (cmd === 'write') return cmdWrite();
  if (cmd === 'adopt') return cmdAdopt();
  if (cmd === 'check' || cmd === 'status') return cmdCheck(rest);
  if (cmd === '-h' || cmd === '--help' || cmd === 'help') {
    console.log('用法：node scripts/build-stamp.mjs [write|check [-v]|adopt]');
    return 0;
  }
  console.error('未知子命令：' + cmd + '（可用：write / check [-v] / adopt）');
  return EXIT_UNKNOWN;
}

main()
  .then((code) => process.exit(code))
  .catch((err) => {
    console.error('✗ 构建指纹工具出错：' + (err && err.message ? err.message : err));
    process.exit(EXIT_UNKNOWN);
  });
