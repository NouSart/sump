import { spawn } from 'node:child_process';
import { cp, mkdtemp, readFile, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const center = ['tests/center.test.ts'];
const spv2 = ['tests/spv2.test.ts'];
const service = ['tests/api.test.ts', 'tests/service.test.ts'];
const mutants = [
  { id: 'TS01', file: 'center.ts', old: "this.spv2Enabled = opts.spv2 ?? process.env.SUMP_SPV2 === '1';", next: 'this.spv2Enabled = opts.spv2 ?? false;', tests: center },
  { id: 'TS02', file: 'center.ts', old: 'if (native !== null && this.connectedFlag) {', next: 'if (native !== null || this.connectedFlag) {', tests: center },
  { id: 'TS03', file: 'center.ts', old: 'if (err instanceof SPv2RemoteError) throw new RemoteError(err.message);', next: 'if (false) throw new RemoteError(err.message);', tests: center },
  { id: 'TS04', file: 'center.ts', old: "if (this.spv2Enabled) {\n      try {\n        return await dialSPv2Once", next: "if (false) {\n      try {\n        return await dialSPv2Once", tests: center },
  { id: 'TS05', file: 'center.ts', old: 'if (!card || !card.addr)', next: 'if (!card && !card.addr)', tests: center },
  { id: 'TS06', file: 'center.ts', old: 'if (err instanceof SPv2UnavailableError) {', next: 'if (false) {', tests: center },
  { id: 'TS07', file: 'center.ts', old: '} else if (err instanceof SPv2RemoteError) {', next: '} else if (false) {', tests: center },
  { id: 'TS08', file: 'center.ts', old: "if (ev.method === 'roster')", next: "if (ev.method !== 'roster')", tests: center },
  { id: 'TS09', file: 'center.ts', old: "if (ev.method === 'settings')", next: "if (ev.method !== 'settings')", tests: center },
  { id: 'TS10', file: 'center.ts', old: "if (!rp || typeof rp !== 'object')", next: 'if (!rp)', tests: center },
  { id: 'TS11', file: 'center.ts', old: 'if (rp.ok !== true)', next: 'if (rp.ok === true)', tests: center },
  { id: 'TS12', file: 'center.ts', old: 'this.backoff = registered ? 1_000 : Math.min(this.backoff * 2, maxBackoff);', next: 'this.backoff = registered ? Math.min(this.backoff * 2, maxBackoff) : 1_000;', tests: center },
  { id: 'TS13', file: 'spv2.ts', old: "if (addr.endsWith('/ws'))", next: "if (!addr.endsWith('/ws'))", tests: spv2 },
  { id: 'TS14', file: 'spv2.ts', old: "if (addr.endsWith('/spv2'))", next: "if (!addr.endsWith('/spv2'))", tests: spv2 },
  { id: 'TS15', file: 'spv2.ts', old: "return addr.replace(/\\/$/, '') + '/spv2';", next: "return addr + '/spv2';", tests: spv2 },
  { id: 'TS16', file: 'spv2.ts', old: 'if (raw.body instanceof Uint8Array) f.body = raw.body;', next: 'if (raw.body instanceof Uint8Array) f.body = new Uint8Array();', tests: spv2 },
  { id: 'TS17', file: 'spv2.ts', old: "if (frame.type !== 'reply' || frame.reply_to !== call.id) return;", next: "if (frame.type !== 'reply' && frame.reply_to !== call.id) return;", tests: spv2 },
  { id: 'TS18', file: 'spv2.ts', old: "if (frame.status === 'error') {\n        const code", next: "if (frame.status !== 'error') {\n        const code", tests: spv2 },
  { id: 'TS19', file: 'spv2.ts', old: "const code = frame.error?.code || 'internal';", next: "const code = frame.error?.code ?? 'internal';", tests: spv2 },
  { id: 'TS20', file: 'spv2.ts', old: 'if (frame.body && frame.body.length > 0) {', next: 'if (frame.body && frame.body.length >= 0) {', tests: spv2 },
  { id: 'TS21', file: 'spv2.ts', old: 'if (this.closedFlag || !this.opened || this.ws.readyState !== WebSocket.OPEN) {', next: 'if (this.closedFlag && !this.opened && this.ws.readyState !== WebSocket.OPEN) {', tests: spv2 },
  { id: 'TS22', file: 'spv2.ts', old: "if (frame.type === 'reply') {", next: "if (frame.type !== 'reply') {", tests: spv2 },
  { id: 'TS23', file: 'spv2.ts', old: "if (frame.status === 'error') {\n        p.reject", next: "if (frame.status !== 'error') {\n        p.reject", tests: spv2 },
  { id: 'TS24', file: 'spv2.ts', old: 'if (frame.body === undefined || frame.body.length === 0) {', next: 'if (frame.body === undefined || frame.body.length > 0) {', tests: spv2 },
  { id: 'TS25', file: 'spv2.ts', old: "if (frame.type === 'event') {", next: "if (frame.type !== 'event') {", tests: spv2 },
  { id: 'TS26', file: 'spv2.ts', old: 'if (frame.body !== undefined && frame.body.length > 0) {', next: 'if (frame.body !== undefined && frame.body.length >= 0) {', tests: spv2 },
  { id: 'TS27', file: 'spv2.ts', old: "if (frame.type !== 'call') return;", next: "if (frame.type === 'call') return;", tests: spv2 },
  { id: 'TS28', file: 'spv2.ts', old: "if (text === '') {", next: "if (text !== '') {", tests: spv2 },
  { id: 'TS29', file: 'spv2.ts', old: "typeof input.conversation_id === 'string' && input.conversation_id !== ''", next: "typeof input.conversation_id === 'string' || input.conversation_id !== ''", tests: spv2 },
  { id: 'TS30', file: 'spv2.ts', old: "if (method === 'approval_pending') {", next: "if (method !== 'approval_pending') {", tests: spv2 },
  { id: 'TS31', file: 'spv2.ts', old: 'const accepted = p !== null ? handlers.onProgress(p) : false;', next: 'const accepted = p !== null ? handlers.onProgress(p) : true;', tests: spv2 },
  { id: 'TS32', file: 'spv2.ts', old: 'const accepted = u !== null ? handlers.onTerminal(u) : false;', next: 'const accepted = u !== null ? handlers.onTerminal(u) : true;', tests: spv2 },
  { id: 'TS33', file: 'spv2.ts', old: "code: 'not_found', message: `unknown method: ${method}`", next: "code: 'invalid_argument', message: `unknown method: ${method}`", tests: spv2 },
  { id: 'TS34', file: 'spv2.ts', old: 'return checkRequestTrust(reqHost, reqOrigin, trust);', next: 'return null;', tests: spv2 },
  { id: 'TS35', file: 'service.ts', old: 'if (!Array.isArray(raw)) return [];', next: 'if (Array.isArray(raw)) return [];', tests: service },
  { id: 'TS36', file: 'service.ts', old: "typeof o?.item_id !== 'string' || o.item_id === '' || typeof o.fire_at !== 'string' || typeof o.ms !== 'number'", next: "typeof o?.item_id !== 'string' && o.item_id === '' && typeof o.fire_at !== 'string' && typeof o.ms !== 'number'", tests: service },
  { id: 'TS37', file: 'service.ts', old: 'if (!isV1 && !isSpv2) {', next: 'if (!isV1 || !isSpv2) {', tests: service },
  { id: 'TS38', file: 'service.ts', old: 'if (denied !== null) {', next: 'if (denied === null) {', tests: service },
  { id: 'TS39', file: 'service.ts', old: 'if (isV1) {', next: 'if (!isV1) {', tests: service },
  { id: 'TS40', file: 'service.ts', old: "if (jp?.action === 'deliver') {", next: "if (jp?.action !== 'deliver') {", tests: service },
];

const args = process.argv.slice(2);
const only = args.find((arg) => arg.startsWith('--only='))?.slice('--only='.length).split(',').filter(Boolean);
const timeout = Number(args.find((arg) => arg.startsWith('--timeout='))?.slice('--timeout='.length) ?? 30_000);
const output = args.find((arg) => arg.startsWith('--output='))?.slice('--output='.length);
const selected = only ? mutants.filter((mutant) => only.includes(mutant.id)) : mutants;
if (selected.length === 0 || (only && selected.length !== only.length)) throw new Error('未知 mutant id');

function runTests(cwd, tests, nativeMode) {
  return new Promise((resolveRun) => {
    const child = spawn(process.execPath, ['--test', ...tests], {
      cwd,
      env: { ...process.env, SUMP_SPV2: nativeMode ? '1' : '' },
      stdio: ['ignore', 'pipe', 'pipe'],
    });
    let log = '';
    let timedOut = false;
    const timer = setTimeout(() => {
      timedOut = true;
      child.kill();
    }, timeout);
    child.stdout.on('data', (chunk) => { log = (log + chunk).slice(-16_000); });
    child.stderr.on('data', (chunk) => { log = (log + chunk).slice(-16_000); });
    child.on('error', (err) => {
      clearTimeout(timer);
      resolveRun({ status: 'killed', log: String(err) });
    });
    child.on('close', (code) => {
      clearTimeout(timer);
      resolveRun({ status: timedOut ? 'timeout' : code === 0 ? 'lived' : 'killed', log });
    });
  });
}

const temp = await mkdtemp(join(tmpdir(), 'sump-frontend-mutation-'));
const results = [];
try {
  await cp(join(root, 'src'), join(temp, 'src'), { recursive: true });
  await cp(join(root, 'tests'), join(temp, 'tests'), { recursive: true });
  await cp(join(root, 'package.json'), join(temp, 'package.json'));
  await symlink(join(root, 'node_modules'), join(temp, 'node_modules'), 'junction');
  const modeFor = (mutant) => (mutant.file === 'service.ts' ? [false] : [false, true]);
  const baselines = new Map();
  for (const mutant of selected) {
    for (const nativeMode of modeFor(mutant)) {
      for (const testFile of mutant.tests) {
        const k = `${testFile}|${String(nativeMode)}`;
        if (!baselines.has(k)) baselines.set(k, { testFile, nativeMode });
      }
    }
  }
  for (const { testFile, nativeMode } of baselines.values()) {
    const baseline = await runTests(temp, [testFile], nativeMode);
    if (baseline.status !== 'lived') {
      console.error(baseline.log);
      throw new Error(`未变异基线未通过（${testFile}, native=${String(nativeMode)}），mutation 结果无效`);
    }
  }
  console.log(`baseline: pass (${[...baselines.keys()].join(', ')})`);
  for (const mutant of selected) {
    const path = join(temp, 'src', 'server', mutant.file);
    const original = await readFile(path, 'utf8');
    if (original.split(mutant.old).length !== 2) throw new Error(`${mutant.id}: 变异锚点不唯一或不存在`);
    try {
      await writeFile(path, original.replace(mutant.old, mutant.next));
      const modes = [];
      for (const nativeMode of modeFor(mutant)) {
        modes.push(await runTests(temp, mutant.tests, nativeMode));
      }
      const status = modes.some((x) => x.status === 'killed')
        ? 'killed'
        : modes.some((x) => x.status === 'timeout')
          ? 'timeout'
          : 'lived';
      results.push({ id: mutant.id, file: mutant.file, old: mutant.old, next: mutant.next, status });
      console.log(`${mutant.id} ${status} ${mutant.file}`);
      if (status === 'lived' && args.includes('--verbose')) {
        for (const mode of modes) console.log(mode.log);
      }
    } finally {
      await writeFile(path, original);
    }
  }
} finally {
  await rm(temp, { recursive: true, force: true });
}

const summary = {
  total: results.length,
  killed: results.filter((result) => result.status === 'killed').length,
  lived: results.filter((result) => result.status === 'lived').length,
  timeout: results.filter((result) => result.status === 'timeout').length,
};
console.log(`summary: ${JSON.stringify(summary)}`);
if (output) await writeFile(resolve(output), `${JSON.stringify({ summary, results }, null, 2)}\n`);
if (results.length !== selected.length || summary.lived + summary.timeout > 0) process.exitCode = 1;
