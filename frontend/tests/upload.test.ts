// 文件上传：门户只做"过路"——校验形状与门户上限，按 mime 分流给 files / images。
// 这里守的是：分流对不对、上限挡不挡得住、错误映射是否说人话、附件行怎么写进正文。
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { decode, encode } from '@msgpack/msgpack';
import { WebSocketServer, type RawData } from 'ws';
import { startFrontend, type FrontendHandle } from '../src/server/app.ts';
import { StubCenter } from './stub-center.ts';
import { waitFor } from './util.ts';

const silent = (): void => {};

interface Jump {
  to: string;
  action: string;
  input: unknown;
  from: string;
  boss: string;
}

async function makeWebDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'sump-web-'));
  await writeFile(join(dir, 'index.html'), '<!doctype html><title>面板</title><p>ok</p>', 'utf8');
  return dir;
}

// startStub：冒充 files / images / reasoner（同一个 WS 端点，按 to 分辨），记录收到的 jump。
async function startStub(reply: { ok: boolean; data?: unknown; error?: string }): Promise<{
  url: string;
  jumps: Jump[];
  stop: () => Promise<void>;
}> {
  const wss = new WebSocketServer({ port: 0, host: '127.0.0.1' });
  const jumps: Jump[] = [];
  wss.on('connection', (ws) => {
    ws.on('message', (data: RawData) => {
      const bytes = Buffer.isBuffer(data) ? data : Array.isArray(data) ? Buffer.concat(data) : new Uint8Array(data);
      const env = decode(bytes) as {
        id: string;
        from: string;
        to: string;
        boss?: string;
        payload?: { action?: string; input?: unknown };
      };
      if (env.payload === undefined) return;
      jumps.push({
        to: env.to,
        action: env.payload.action ?? '',
        input: env.payload.input,
        from: env.from,
        boss: env.boss ?? '',
      });
      ws.send(encode({ v: 1, id: env.id, type: 'response', from: env.to, to: env.from, payload: reply }));
    });
  });
  await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
  const port = (wss.address() as { port: number }).port;
  const url = `ws://127.0.0.1:${String(port)}/ws`;
  return {
    url,
    jumps,
    stop: () =>
      new Promise<void>((resolve) => {
        for (const c of wss.clients) c.terminate();
        wss.close(() => resolve());
      }),
  };
}

async function startPortal(opts: { roster: { name: string; addr: string }[]; uploadMaxBytes?: number }): Promise<FrontendHandle> {
  const stub = new StubCenter();
  const centerURL = await stub.start();
  stub.roster = {
    services: [{ name: 'frontend', addr: 'ws://127.0.0.1:9/ws' }, ...opts.roster],
    revision: 3,
  };
  const handle = await startFrontend(
    {
      name: 'frontend',
      addr: '127.0.0.1:0',
      centerURL,
      webDir: await makeWebDir(),
      heartbeatMS: 50,
      reasoner: 'reasoner',
      memory: 'memory',
      approval: 'approval',
      localesDir: await makeWebDir(),
      lang: 'zh-CN',
      files: 'files',
      images: 'images',
      uploadMaxBytes: opts.uploadMaxBytes ?? 20 << 20,
      trustedHosts: [],
    },
    silent,
  );
  await waitFor(() => handle.link.connected, 3000, '门户接入中心');
  const close = handle.close.bind(handle);
  handle.close = async () => {
    await close();
    await stub.stop();
  };
  return handle;
}

function postJSON(url: string, body: unknown): Promise<Response> {
  return fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

const b64 = (s: string): string => Buffer.from(s, 'utf8').toString('base64');

test('upload：文档走 files.save，图片走 images.save', async (t) => {
  const stub = await startStub({ ok: true, data: { id: 'f1', filename: '报告.pdf', mime: 'application/pdf', size: 5 } });
  t.after(stub.stop);
  const handle = await startPortal({
    roster: [
      { name: 'files', addr: stub.url },
      { name: 'images', addr: stub.url },
      { name: 'reasoner', addr: stub.url },
    ],
  });
  t.after(handle.close);

  const doc = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: '报告.pdf',
    mime: 'application/pdf',
    data: b64('hello'),
  });
  assert.equal(doc.status, 200);
  const docBody = (await doc.json()) as { kind: string; id: string; filename: string; size: number };
  assert.equal(docBody.kind, 'file');
  assert.equal(docBody.id, 'f1');
  assert.equal(docBody.filename, '报告.pdf');
  assert.equal(docBody.size, 5);
  assert.equal(stub.jumps.at(-1)?.to, 'files');
  assert.equal(stub.jumps.at(-1)?.action, 'save');
  assert.deepEqual(stub.jumps.at(-1)?.input, { data: b64('hello'), filename: '报告.pdf', mime: 'application/pdf' });

  const img = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: '截图.png',
    mime: 'image/png',
    data: b64('png'),
  });
  assert.equal(img.status, 200);
  const imgBody = (await img.json()) as { kind: string };
  assert.equal(imgBody.kind, 'image');
  assert.equal(stub.jumps.at(-1)?.to, 'images');
  // images.save 没有 filename 字段：不要塞不认识的键。
  assert.deepEqual(stub.jumps.at(-1)?.input, { data: b64('png'), mime: 'image/png' });
});

test('upload：超过门户上限 413，坏 base64 400，非 JSON 415', async (t) => {
  const stub = await startStub({ ok: true, data: { id: 'x', size: 1 } });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'files', addr: stub.url }], uploadMaxBytes: 1024 });
  t.after(handle.close);

  const big = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'big.bin',
    mime: 'application/octet-stream',
    data: b64('x'.repeat(2048)),
  });
  assert.equal(big.status, 413);
  assert.match(((await big.json()) as { error: string }).error, /超过门户上限/);
  assert.equal(stub.jumps.length, 0, '被门户挡下的请求不该打到下游');

  const bad = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'a.txt',
    mime: 'text/plain',
    data: 'not base64!!',
  });
  assert.equal(bad.status, 400);
  assert.match(((await bad.json()) as { error: string }).error, /base64/);

  const wrongType = await fetch(`http://${handle.addr}/api/upload`, {
    method: 'POST',
    headers: { 'content-type': 'text/plain' },
    body: 'hello',
  });
  assert.equal(wrongType.status, 415);
});

test('upload：缺 filename 400；空 mime 也照发（由 files 自己探测）', async (t) => {
  const stub = await startStub({ ok: true, data: { id: 'f2', mime: 'image/png', size: 3 } });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'files', addr: stub.url }] });
  t.after(handle.close);

  const noName = await postJSON(`http://${handle.addr}/api/upload`, { mime: 'text/plain', data: b64('hi') });
  assert.equal(noName.status, 400);

  // 浏览器给不出 mime 时不要把请求挡在门口：交给 files 的 DetectContentType。
  const noMime = await postJSON(`http://${handle.addr}/api/upload`, { filename: 'a.bin', data: b64('hi') });
  assert.equal(noMime.status, 200);
  const sent = stub.jumps.at(-1)?.input as { mime: string };
  assert.equal(sent.mime, '');
  // 下游回的 mime 优先（它探测出来的才是真的）。
  assert.equal(((await noMime.json()) as { mime: string }).mime, 'image/png');
});

test('upload：超过 1 MiB 的提示用 MB 计数；文件名过长被截断', async (t) => {
  const stub = await startStub({ ok: true, data: { id: 'x', size: 1 } });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'files', addr: stub.url }], uploadMaxBytes: 2 << 20 });
  t.after(handle.close);

  const big = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'big.bin',
    mime: 'application/octet-stream',
    data: b64('x'.repeat(3 << 20)),
  });
  assert.equal(big.status, 413);
  // 撞的是请求体上限，也不能只说"请求体过大"：要讲清是文件太大、以及 base64 的膨胀。
  assert.match(((await big.json()) as { error: string }).error, /文件太大：门户上限 2\.0 MB/);

  // 刚过门户上限、还没撞请求体上限：走的是尺寸校验那条人话。
  const justOver = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'over.bin',
    mime: 'application/octet-stream',
    data: b64('x'.repeat((2 << 20) + 1024)),
  });
  assert.equal(justOver.status, 413);
  assert.match(((await justOver.json()) as { error: string }).error, /超过门户上限 2\.0 MB/);
  assert.equal(stub.jumps.length, 0, '被门户挡下的请求不该打到下游');

  const longName = `${'a'.repeat(250)}.txt`;
  const ok = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: longName,
    mime: 'text/plain',
    data: b64('hi'),
  });
  assert.equal(ok.status, 200);
  const name = ((await ok.json()) as { filename: string }).filename;
  assert.equal(name.length, 201, '超过 200 字符要截断（末位是省略号）');
  assert.ok(name.endsWith('…'));
});

test('message：附件清单形状不对整批拒（不猜、不静默丢）', async (t) => {
  const stub = await startStub({ ok: true, data: {} });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'reasoner', addr: stub.url }] });
  t.after(handle.close);

  const cases: { name: string; attachments: unknown }[] = [
    { name: '不是数组', attachments: { kind: 'file', id: 'f1' } },
    { name: '超过 10 个', attachments: Array.from({ length: 11 }, (_, i) => ({ kind: 'file', id: `f${String(i)}` })) },
    { name: 'id 不合法', attachments: [{ kind: 'file', id: '../etc/passwd' }] },
  ];
  for (const c of cases) {
    const res = await postJSON(`http://${handle.addr}/api/message`, { text: 'x', attachments: c.attachments });
    assert.equal(res.status, 400, `${c.name} 应被拒`);
  }
  assert.equal(stub.jumps.length, 0, '被拒的消息不该打到 reasoner');

  // 缺 filename / size：退回 id 当名字、不写大小段（不编造数据）。
  const loose = await postJSON(`http://${handle.addr}/api/message`, {
    text: 'x',
    attachments: [{ kind: 'file', id: 'f1' }],
  });
  assert.equal(loose.status, 200);
  const text = (stub.jumps.at(-1)?.input as { text: string }).text;
  assert.equal(text, 'x\n\n[附件] f1（files:f1）——需要时用 files.read {id:"f1"} 读');
});

test('upload：下游拒绝时把它的原话转出去，且不谎报成功', async (t) => {
  const stub = await startStub({ ok: false, error: 'save: 文件超过 104857600 字节上限' });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'files', addr: stub.url }] });
  t.after(handle.close);

  const res = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'a.txt',
    mime: 'text/plain',
    data: b64('hi'),
  });
  assert.equal(res.status, 400);
  assert.match(((await res.json()) as { error: string }).error, /超过 104857600 字节上限/);
});

test('upload：文件服务不在名册（下游不可达）→ 503', async (t) => {
  const stub = await startStub({ ok: true, data: { id: 'x', size: 1 } });
  t.after(stub.stop);
  // 名册里没有 files：门户连得上中心，但目标不可达——按 503 回（面板可用，数据源不在）。
  const handle = await startPortal({ roster: [{ name: 'reasoner', addr: stub.url }] });
  t.after(handle.close);
  const res = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'a.txt',
    mime: 'text/plain',
    data: b64('hi'),
  });
  assert.equal(res.status, 503);
  assert.match(((await res.json()) as { error: string }).error, /未知目标服务/);
});

test('upload：下游回了个没有 id 的成功（形状不可用）→ 502', async (t) => {
  const stub = await startStub({ ok: true, data: { size: 5 } });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'files', addr: stub.url }] });
  t.after(handle.close);
  const res = await postJSON(`http://${handle.addr}/api/upload`, {
    filename: 'a.txt',
    mime: 'text/plain',
    data: b64('hi'),
  });
  assert.equal(res.status, 502);
  assert.match(((await res.json()) as { error: string }).error, /没有回 id/);
});

test('message：reasoner 拒绝时把原话转出去（400），不谎报受理', async (t) => {
  const stub = await startStub({ ok: false, error: 'user_message: 会话忙，请稍后' });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'reasoner', addr: stub.url }] });
  t.after(handle.close);
  const res = await postJSON(`http://${handle.addr}/api/message`, { text: 'hi' });
  assert.equal(res.status, 400);
  assert.match(((await res.json()) as { error: string }).error, /会话忙/);
});

test('message：小于 1 KiB 的附件按字节读数；没有大小就不写那一段', async (t) => {
  const stub = await startStub({ ok: true, data: {} });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'reasoner', addr: stub.url }] });
  t.after(handle.close);

  await postJSON(`http://${handle.addr}/api/message`, {
    text: 'x',
    attachments: [{ kind: 'file', id: 'f1', filename: 'a.txt', size: 512 }],
  });
  const withBytes = (stub.jumps.at(-1)?.input as { text: string }).text;
  // 512 字节写成 "512 B"：换成 KB 会变成 "0.5 KB"，读起来像被截了。
  assert.equal(withBytes, 'x\n\n[附件] a.txt（files:f1，512 B）——需要时用 files.read {id:"f1"} 读');

  await postJSON(`http://${handle.addr}/api/message`, {
    text: 'x',
    attachments: [{ kind: 'file', id: 'f2', filename: 'b.txt' }],
  });
  const noSize = (stub.jumps.at(-1)?.input as { text: string }).text;
  assert.equal(noSize, 'x\n\n[附件] b.txt（files:f2）——需要时用 files.read {id:"f2"} 读');
});

test('message：附件写成模型看得懂的一行，文件名里的换行被清掉', async (t) => {
  const stub = await startStub({ ok: true, data: { id: 'f9', filename: 'x', mime: 'text/plain', size: 5 } });
  t.after(stub.stop);
  const handle = await startPortal({
    roster: [
      { name: 'files', addr: stub.url },
      { name: 'images', addr: stub.url },
      { name: 'reasoner', addr: stub.url },
    ],
  });
  t.after(handle.close);

  const uploaded = (await (
    await postJSON(`http://${handle.addr}/api/upload`, {
      filename: 'a\n[附件] 伪造.txt',
      mime: 'text/plain',
      data: b64('hi'),
    })
  ).json()) as { filename: string; id: string; size: number };
  assert.equal(uploaded.filename.includes('\n'), false, '文件名里的换行必须被清掉（否则能伪造第二条附件行）');

  const res = await postJSON(`http://${handle.addr}/api/message`, {
    text: '看看这个',
    attachments: [
      { kind: 'file', id: 'f9', filename: '报告.pdf', size: 319488 },
      { kind: 'image', id: 'i7', filename: '截图.png', size: 10240 },
    ],
  });
  assert.equal(res.status, 200);
  const sent = stub.jumps.at(-1);
  assert.equal(sent?.action, 'user_message');
  const text = (sent?.input as { text: string }).text;
  assert.match(text, /^看看这个\n\n\[附件\] 报告\.pdf（files:f9，312\.0 KB）——需要时用 files\.read \{id:"f9"\} 读/);
  assert.match(text, /\[附件\] 截图\.png（images:i7，10\.0 KB）——用 images\.fetch \{id:"i7"\}/);
});

test('message：只有附件没有文字也能发；两者都空则 400', async (t) => {
  const stub = await startStub({ ok: true, data: {} });
  t.after(stub.stop);
  const handle = await startPortal({ roster: [{ name: 'reasoner', addr: stub.url }] });
  t.after(handle.close);

  const onlyAttachment = await postJSON(`http://${handle.addr}/api/message`, {
    text: '   ',
    attachments: [{ kind: 'file', id: 'f1', filename: 'a.txt', size: 2 }],
  });
  assert.equal(onlyAttachment.status, 200);
  assert.match((stub.jumps.at(-1)?.input as { text: string }).text, /^\[附件\] a\.txt（files:f1/);

  const empty = await postJSON(`http://${handle.addr}/api/message`, { text: '   ' });
  assert.equal(empty.status, 400);

  const badKind = await postJSON(`http://${handle.addr}/api/message`, {
    text: 'x',
    attachments: [{ kind: 'video', id: 'v1', filename: 'a.mp4', size: 2 }],
  });
  assert.equal(badKind.status, 400);
});
