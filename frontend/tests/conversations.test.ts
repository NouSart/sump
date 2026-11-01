// 多会话：门户是"一个用户 + 多个会话"，会话本身住在记忆服务（它的一行），门户只做翻译。
//
// 这里守的是三件事：
//   1. 列表**列所有入口的会话**（门户 / QQ 私聊 / QQ 群都是这台机器主人的对话），
//      但别的入口的会话带 `readonly`：能看，不能在门户里替它说话；
//   2. 写（发消息 / 终端 / 改名 / 归档）只接受门户自己的会话号——
//      不设限就能借门户往 qq:private:* 里塞消息；读则按 id 原样透传；
//   3. 终端命令的归属会话要记住（shell 的补报里没有会话 id）：
//      补报回到门户时既要带 conversationId 推给浏览器，也要入账到同一个会话。
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { request as httpRequest } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { decode, encode } from '@msgpack/msgpack';
import { WebSocket, WebSocketServer, type RawData } from 'ws';
import { startFrontend, type FrontendHandle } from '../src/server/app.ts';
import { DEFAULT_CONVERSATION_ID, isPortalConversationId, newPortalConversationId } from '../src/server/portal.ts';
import { StubCenter } from './stub-center.ts';
import { waitFor } from './util.ts';

const silent = (): void => {};

interface Jump {
  to: string;
  action: string;
  input: unknown;
}

async function makeWebDir(): Promise<string> {
  const dir = await mkdtemp(join(tmpdir(), 'sump-web-'));
  await writeFile(join(dir, 'index.html'), '<!doctype html><title>面板</title><p>ok</p>', 'utf8');
  return dir;
}

// startStub 冒充 reasoner / memory / shell（同一个 WS 端点，按 to 分辨）。
// replyFor 允许按动作给不同答复（会话列表与「跳过去」需要的形状不一样）。
async function startStub(replyFor: (action: string, input: unknown) => unknown): Promise<{
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
        payload?: { action?: string; input?: unknown };
      };
      if (env.payload === undefined) return;
      const action = env.payload.action ?? '';
      jumps.push({ to: env.to, action, input: env.payload.input });
      ws.send(
        encode({
          v: 1,
          id: env.id,
          type: 'response',
          from: env.to,
          to: env.from,
          payload: { ok: true, data: replyFor(action, env.payload.input) },
        }),
      );
    });
  });
  await new Promise<void>((resolve) => wss.once('listening', () => resolve()));
  const port = (wss.address() as { port: number }).port;
  return {
    url: `ws://127.0.0.1:${port}/ws`,
    jumps,
    stop: () =>
      new Promise<void>((resolve) => {
        for (const c of wss.clients) c.terminate();
        wss.close(() => resolve());
      }),
  };
}

async function startPortal(roster: { name: string; addr: string }[]): Promise<{ handle: FrontendHandle; center: StubCenter }> {
  const center = new StubCenter();
  const centerURL = await center.start();
  center.roster = { services: [{ name: 'frontend', addr: 'ws://127.0.0.1:9/ws' }, ...roster], revision: 3 };
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
      shell: 'shell',
      trustedHosts: [],
    },
    silent,
  );
  await waitFor(() => handle.link.connected, 3000, '门户接入中心');
  return { handle, center };
}

function postJSON(url: string, body: unknown): Promise<Response> {
  return fetch(url, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify(body),
  });
}

// fireReport 冒充 shell，向门户送一条 report_result。
async function fireReport(portalAddr: string, input: unknown): Promise<void> {
  const ws = new WebSocket(`ws://${portalAddr}/ws`);
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => {
      resolve();
    });
    ws.once('error', reject);
  });
  ws.send(
    encode({
      v: 1,
      id: 'r1',
      type: 'jump',
      from: 'shell',
      to: 'frontend',
      payload: { action: 'report_result', input },
    }),
  );
  await new Promise<void>((resolve) => {
    ws.once('message', () => {
      resolve();
    });
  });
  ws.terminate();
}

test('会话 id：门户的形状（默认会话 + 带后缀的新会话），别的一律不认', () => {
  assert.equal(isPortalConversationId(DEFAULT_CONVERSATION_ID), true);
  assert.equal(isPortalConversationId(newPortalConversationId()), true);
  // 边界：别的入口的会话不是门户的（这行断言就是隐私边界本身）。
  assert.equal(isPortalConversationId('qq:private:42'), false);
  assert.equal(isPortalConversationId('web:private:ownerX'), false);
  assert.equal(isPortalConversationId(''), false);
  // 新 id 每次都不同（同一毫秒内也不会撞）。
  const a = newPortalConversationId(1, () => 0.5);
  const b = newPortalConversationId(1, () => 0.25);
  assert.notEqual(a, b);
});

test('GET /api/conversations：列出所有入口的会话，别的入口的标成只读', async (t) => {
  const memory = await startStub((action) => {
    assert.equal(action, 'conversation_list');
    return {
      conversations: [
        { id: 'web:private:owner-abc', title: '门户新会话', entry: 'web', status: 'active', updated_at: '2026-11-01T10:00:00Z' },
        { id: 'qq:private:42', title: 'QQ 私聊', entry: 'qq', status: 'active', updated_at: '2026-11-01T09:00:00Z' },
      ],
    };
  });
  t.after(memory.stop);
  const { handle, center } = await startPortal([{ name: 'memory', addr: memory.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const res = await fetch(`http://${handle.addr}/api/conversations`);
  assert.equal(res.status, 200);
  const body = (await res.json()) as { conversations: { id: string; readonly?: boolean }[] };
  const ids = body.conversations.map((c) => c.id);
  // QQ 里的对话也在这台机器上发生：门户是主人自己开的界面，看得见才知道群里发生了什么。
  assert.deepEqual(ids, ['web:private:owner-abc', 'qq:private:42', DEFAULT_CONVERSATION_ID]);
  assert.equal(body.conversations.find((c) => c.id === 'qq:private:42')?.readonly, true, 'qq 的会话只能看');
  assert.equal(body.conversations.find((c) => c.id === 'web:private:owner-abc')?.readonly, false);
  assert.equal(body.conversations.find((c) => c.id === DEFAULT_CONVERSATION_ID)?.readonly, false);

  // 归档的会话默认不列，include_archived=1 时透传给记忆服务。
  await fetch(`http://${handle.addr}/api/conversations?include_archived=1`);
  const last = memory.jumps.at(-1)?.input as { include_archived?: boolean };
  assert.equal(last.include_archived, true);
});

test('POST /api/conversations：新建会话用门户的 id，标题截到 60 字', async (t) => {
  const memory = await startStub((action, input) => {
    assert.equal(action, 'conversation_open');
    const p = input as { id: string; title: string; entry: string };
    return { conversation: { id: p.id, title: p.title, entry: p.entry, status: 'active' } };
  });
  t.after(memory.stop);
  const { handle, center } = await startPortal([{ name: 'memory', addr: memory.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const res = await postJSON(`http://${handle.addr}/api/conversations`, { title: '题'.repeat(80) });
  assert.equal(res.status, 200);
  const body = (await res.json()) as { conversation: { id: string; title: string; entry: string } };
  assert.equal(isPortalConversationId(body.conversation.id), true);
  assert.equal(body.conversation.entry, 'web');
  assert.equal([...body.conversation.title].length, 60);
});

test('POST /api/conversations/rename：改名透传；别的入口的会话一律 400 且不跳', async (t) => {
  const memory = await startStub((action, input) => {
    const p = input as { id: string; title: string };
    return { conversation: { id: p.id, title: p.title, status: 'active' } };
  });
  t.after(memory.stop);
  const { handle, center } = await startPortal([{ name: 'memory', addr: memory.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const ok = await postJSON(`http://${handle.addr}/api/conversations/rename`, {
    conversation_id: `${DEFAULT_CONVERSATION_ID}-x1`,
    title: '  改名了  ',
  });
  assert.equal(ok.status, 200);
  assert.deepEqual(memory.jumps.at(-1)?.input, { id: `${DEFAULT_CONVERSATION_ID}-x1`, title: '改名了' });

  const before = memory.jumps.length;
  const bad = await postJSON(`http://${handle.addr}/api/conversations/rename`, {
    conversation_id: 'qq:private:42',
    title: '偷改别人的会话',
  });
  assert.equal(bad.status, 400);
  assert.match(((await bad.json()) as { error: string }).error, /不属于门户/);
  assert.equal(memory.jumps.length, before, '被拒的请求不该真的跳出去');
});

test('POST /api/conversations/archive：默认会话不可归档，其余透传', async (t) => {
  const memory = await startStub((action, input) => {
    const p = input as { id: string; archived: boolean };
    return { conversation: { id: p.id, status: p.archived ? 'archived' : 'active' } };
  });
  t.after(memory.stop);
  const { handle, center } = await startPortal([{ name: 'memory', addr: memory.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const bad = await postJSON(`http://${handle.addr}/api/conversations/archive`, {
    conversation_id: DEFAULT_CONVERSATION_ID,
  });
  assert.equal(bad.status, 400);
  assert.equal(memory.jumps.length, 0);

  const ok = await postJSON(`http://${handle.addr}/api/conversations/archive`, {
    conversation_id: `${DEFAULT_CONVERSATION_ID}-y2`,
  });
  assert.equal(ok.status, 200);
  assert.deepEqual(memory.jumps.at(-1)?.input, { id: `${DEFAULT_CONVERSATION_ID}-y2`, archived: true });

  // archived:false = 取消归档。
  await postJSON(`http://${handle.addr}/api/conversations/archive`, {
    conversation_id: `${DEFAULT_CONVERSATION_ID}-y2`,
    archived: false,
  });
  assert.deepEqual(memory.jumps.at(-1)?.input, { id: `${DEFAULT_CONVERSATION_ID}-y2`, archived: false });
});

test('消息 / 历史 / 任务都按会话走：读能看别的入口，写只在门户自己的会话', async (t) => {
  const reasoner = await startStub(() => ({ ok: true }));
  t.after(reasoner.stop);
  const memory = await startStub((action) => {
    if (action === 'history') return { messages: [] };
    return { tasks: [] };
  });
  t.after(memory.stop);
  const { handle, center } = await startPortal([
    { name: 'reasoner', addr: reasoner.url },
    { name: 'memory', addr: memory.url },
  ]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  // 缺省 = 默认会话（向后兼容：老界面不带这个字段也能用）。
  await postJSON(`http://${handle.addr}/api/message`, { text: '你好' });
  assert.equal((reasoner.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, DEFAULT_CONVERSATION_ID);
  await fetch(`http://${handle.addr}/api/history?limit=5`);
  assert.equal((memory.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, DEFAULT_CONVERSATION_ID);
  await fetch(`http://${handle.addr}/api/tasks`);
  assert.equal((memory.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, DEFAULT_CONVERSATION_ID);

  // 指定会话：原样透传。
  const custom = `${DEFAULT_CONVERSATION_ID}-z3`;
  const sent = await postJSON(`http://${handle.addr}/api/message`, { text: '你好', conversation_id: custom });
  assert.equal(sent.status, 200);
  assert.equal((reasoner.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, custom);
  const hist = await fetch(`http://${handle.addr}/api/history?conversation_id=${custom}`);
  assert.equal(((await hist.json()) as { conversationId: string }).conversationId, custom);
  await fetch(`http://${handle.addr}/api/tasks?conversation_id=${custom}`);
  assert.equal((memory.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, custom);

  // 别的入口的会话：**读**可以（门户里要能看见 QQ 里发生了什么），**写**不行
  //（回复走哪扇门回哪扇门，门户不替 QQ 说话）。
  const beforeR = reasoner.jumps.length;
  const write = await postJSON(`http://${handle.addr}/api/message`, { text: '你好', conversation_id: 'qq:private:42' });
  assert.equal(write.status, 400);
  assert.match(((await write.json()) as { error: string }).error, /不属于门户/);
  assert.equal(reasoner.jumps.length, beforeR, '被拒的写入不该跳出去');

  const readHist = await fetch(`http://${handle.addr}/api/history?conversation_id=qq:private:42`);
  assert.equal(readHist.status, 200);
  assert.equal((memory.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, 'qq:private:42');
  const readTasks = await fetch(`http://${handle.addr}/api/tasks?conversation_id=qq:group:67890`);
  assert.equal(readTasks.status, 200);
  assert.equal((memory.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, 'qq:group:67890');

  // 会话号不是随便什么都行：形状不对就明确拒绝，不把奇怪的东西传下去。
  const badShape = await fetch(`http://${handle.addr}/api/history?conversation_id=${encodeURIComponent('bad id!')}`);
  assert.equal(badShape.status, 400);

  // 恢复任务是在改「那个会话的当前任务指针」——属于写，只认门户自己的会话。
  const resumeForeign = await postJSON(`http://${handle.addr}/api/tasks/resume`, {
    task_id: 'task-7',
    conversation_id: 'qq:group:67890',
  });
  assert.equal(resumeForeign.status, 400);
  assert.match(((await resumeForeign.json()) as { error: string }).error, /不属于门户/);
  assert.ok(
    memory.jumps.every((j) => j.action !== 'task_resume'),
    '被拒的恢复不该跳到记忆服务',
  );
  const resumeOwn = await postJSON(`http://${handle.addr}/api/tasks/resume`, {
    task_id: 'task-8',
    conversation_id: custom,
  });
  assert.equal(resumeOwn.status, 200);
  assert.equal(memory.jumps.at(-1)?.action, 'task_resume');
  assert.equal((memory.jumps.at(-1)?.input as { task_id: string }).task_id, 'task-8');
});

// 重试：失败的那一轮可能发生在 QQ 会话里（入口自己才是那扇门），门户只是把请求转过去；
// 但会话号仍然要过形状校验——它会被原样放进跳转。
test('POST /api/retry：转给 reasoner，会话号要合法', async (t) => {
  const reasoner = await startStub((action) => {
    assert.equal(action, 'retry');
    return { retried: true };
  });
  t.after(reasoner.stop);
  const { handle, center } = await startPortal([{ name: 'reasoner', addr: reasoner.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const ok = await postJSON(`http://${handle.addr}/api/retry`, { conversation_id: 'qq:group:67890' });
  assert.equal(ok.status, 200);
  assert.deepEqual(await ok.json(), { retried: true });
  assert.equal((reasoner.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, 'qq:group:67890');

  // 不带会话号 = 门户自己的默认会话（老界面）。
  await postJSON(`http://${handle.addr}/api/retry`, {});
  assert.equal((reasoner.jumps.at(-1)?.input as { conversation_id: string }).conversation_id, DEFAULT_CONVERSATION_ID);

  const before = reasoner.jumps.length;
  const bad = await postJSON(`http://${handle.addr}/api/retry`, { conversation_id: 'bad id!' });
  assert.equal(bad.status, 400);
  assert.equal(reasoner.jumps.length, before, '形状不对的会话号不该跳出去');
});

test('终端：命令发起的会话被记住，补报按该会话推送并入账', async (t) => {
  let item = 0;
  const stub = await startStub((action) => {
    if (action === 'run') {
      item++;
      return { item_id: `sh-${String(item)}`, phase: 'queued' };
    }
    return {};
  });
  t.after(stub.stop);
  const { handle, center } = await startPortal([
    { name: 'shell', addr: stub.url },
    { name: 'memory', addr: stub.url },
  ]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });

  const custom = `${DEFAULT_CONVERSATION_ID}-t9`;
  const res = await postJSON(`http://${handle.addr}/api/terminal`, { command: 'echo hi', conversation_id: custom });
  assert.equal(res.status, 200);
  const body = (await res.json()) as { itemId: string };
  assert.equal(body.itemId, 'sh-1');

  await fireReport(handle.addr, {
    item_id: 'sh-1',
    phase: 'done',
    final: true,
    data: { command: 'echo hi', exit_code: 0, stdout: 'hi\n', stderr: '', duration_ms: 3 },
  });

  await waitFor(
    () => stub.jumps.some((j) => j.action === 'store'),
    3000,
    '终端结果入账',
  );
  const store = stub.jumps.find((j) => j.action === 'store')?.input as { conversation_id: string };
  assert.equal(store.conversation_id, custom, '入账要落在发起这条命令的会话里');

  // 没发起过（或门户重启过）的 item_id：归到默认会话，而不是丢进某个随机会话。
  await fireReport(handle.addr, {
    item_id: 'sh-unknown',
    phase: 'done',
    final: true,
    data: { command: 'pwd', exit_code: 0, stdout: '/x\n', stderr: '', duration_ms: 1 },
  });
  await waitFor(
    () => stub.jumps.filter((j) => j.action === 'store').length >= 2,
    3000,
    '未知归属的补报也要入账',
  );
  const second = stub.jumps.filter((j) => j.action === 'store')[1]?.input as { conversation_id: string };
  assert.equal(second.conversation_id, DEFAULT_CONVERSATION_ID);
});

test('终端：非门户会话的命令请求 400', async (t) => {
  const stub = await startStub(() => ({ item_id: 'sh-x', phase: 'queued' }));
  t.after(stub.stop);
  const { handle, center } = await startPortal([{ name: 'shell', addr: stub.url }]);
  t.after(async () => {
    await handle.close();
    await center.stop();
  });
  const res = await postJSON(`http://${handle.addr}/api/terminal`, {
    command: 'rm -rf /',
    conversation_id: 'qq:private:42',
  });
  assert.equal(res.status, 400);
  assert.equal(stub.jumps.length, 0);
});
