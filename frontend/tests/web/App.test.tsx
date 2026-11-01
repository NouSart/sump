// App 集成测试（逻辑层）：首屏加载 / 服务切换 / 写入→待确认→applied 事件（toast+状态回落）/
// 失败事件（未送达+原因）/ 写入请求失败（编辑态保留）/ roster revision 触发重拉 /
// status 事件 / toast 自动消失（假时钟）/ 外观开关（属性 + 本地记忆）。
//
// 说明：SSE 与 fetch 都在边界处替换（假 EventSource / fetch 路由桩），App 代码零改动。
import assert from 'node:assert/strict';
import { describe, it, expect, vi } from 'vitest';
import { act, render, screen, fireEvent, waitFor, within } from '@testing-library/react';
import { App } from '../../src/web/App';
import { remainingText } from '../../src/web/components/PortalView';
import type { RosterView } from '../../src/web/types';
import {
  countCalls,
  installFakeEventSource,
  jsonResponse,
  latestSource,
  overviewOf,
  rosterOf,
  settingsOf,
  stubFetch,
} from './harness';

const okOverview = (r: RosterView) =>
  overviewOf(r, [settingsOf('images', [{ key: 'max_mb', value: '1', default: '32', overridden: true }])]);

// PORTAL_CONVERSATION 是门户默认会话（首屏拉到它之后才会去拉任务与记录）。
function PORTAL_CONVERSATION() {
  return {
    id: 'web:private:owner',
    title: '',
    entry: 'web',
    status: 'active' as const,
    created_at: '2026-10-17T09:00:00Z',
    updated_at: '2026-10-17T10:00:00Z',
  };
}

// boot 安装替身并挂载 App：
//   - setResponse 可替换 set_setting 的应答（默认成功）；
//   - messageResponse 可替换 /api/message 的应答（默认受理）；
//   - history 可替换 /api/history 的历史（默认空）；
//   - openSettings=false 停在对话视图（设置从输入框的「设置」进入）；
//   - 返回的 setRoster 用于让 overview 桩跟随名册推进（与服务端一致：名册更新后重拉应见到新名册）。
async function boot(
  over: {
    setResponse?: () => Response;
    messageResponse?: () => Response;
    openSettings?: boolean;
    select?: string | null;
    history?: { role: string; content: string; at: string; task_id?: string }[];
    tasks?: { id: string; name: string; status: 'active' | 'done'; updated_at: string }[];
    conversations?: { id: string; title: string; status: 'active' | 'archived'; updated_at: string }[];
  } = {},
) {
  let currentRoster = rosterOf(['images', 'qq']);
  const taskList =
    over.tasks ??
    [{ id: 'task-1', name: '看看现在几点', status: 'active' as const, updated_at: '2026-10-17T10:00:00Z' }];
  const calls = stubFetch((url, init) => {
    if (url === '/api/overview') return jsonResponse(okOverview(currentRoster));
    // 门户首屏先拉会话列表，再按当前会话拉任务与记录。
    if (url === '/api/conversations') {
      return jsonResponse({ conversations: over.conversations ?? [PORTAL_CONVERSATION()] });
    }
    if (url.startsWith('/api/tasks?') || url === '/api/tasks') {
      return jsonResponse({
        conversationId: 'web:private:owner',
        tasks: taskList.map((x) => ({ conversation_id: 'web:private:owner', created_at: x.updated_at, ...x })),
      });
    }
    if (url.startsWith('/api/history')) {
      return jsonResponse({ conversationId: 'web:private:owner', messages: over.history ?? [] });
    }
    if (url === '/api/message') {
      if (over.messageResponse) return over.messageResponse();
      return jsonResponse({ ok: true, conversationId: 'web:private:owner' });
    }
    if (url === '/api/tasks/resume') {
      const id = JSON.parse(String(init?.body ?? '{}')).task_id as string;
      return jsonResponse({
        task: { id, status: 'active' },
        messages: [
          { role: 'user', content: '恢复前的提问', at: '2026-10-17T11:00:30Z', seq: 6, task_id: id },
          { role: 'assistant', content: '恢复出来的记录', at: '2026-10-17T11:01:00Z', seq: 7, task_id: id },
        ],
      });
    }
    if (url === '/api/settings/set') {
      if (over.setResponse) return over.setResponse();
      return jsonResponse({
        ok: true,
        services: [settingsOf('images', [{ key: 'max_mb', value: '2', default: '32', overridden: true }])],
      });
    }
    if (url === '/api/settings/reset') {
      return jsonResponse({
        ok: true,
        services: [settingsOf('images', [{ key: 'max_mb', value: '32', default: '32', overridden: false }])],
      });
    }
    return undefined;
  });
  installFakeEventSource();
  render(<App />);
  if (over.openSettings !== false) {
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    // 门户现在排第一并默认选中；多数用例仍以 images 的设置项为主体，这里代为切换。
    // select=null 时留在门户页，等外观设置渲染出来再返回。
    const select = over.select === undefined ? 'images' : over.select;
    if (select === null) {
      await screen.findByRole('slider', { name: '主题时段' });
    } else {
      fireEvent.click(await screen.findByText(select));
    }
  }
  return {
    calls,
    setRoster: (r: RosterView) => {
      currentRoster = r;
    },
  };
}

// writeNewValue 在 images 的第一行进入编辑、改值并保存。
function writeNewValue(value: string): void {
  fireEvent.click(screen.getAllByRole('button', { name: '修改' })[0]!);
  fireEvent.change(screen.getByLabelText('max_mb 修改'), { target: { value } });
  fireEvent.click(screen.getByRole('button', { name: '保存' }));
}

// emitSSE 在 act 中触发一个 SSE 事件。
async function emitSSE(type: string, data: unknown): Promise<void> {
  await act(async () => {
    latestSource().emit(type, data);
  });
}

describe('App', () => {
  it('首屏：门户排第一并默认选中；名册/状态/角标；切换服务联动面板', async () => {
    await boot({ select: null });
    await screen.findByRole('heading', { name: 'frontend' }); // 门户默认选中
    assert.ok(screen.getByText('服务 · 3'));
    assert.ok(screen.getByRole('img', { name: '已连接设置中心' }));
    assert.ok(screen.getByTitle('1 个设置项')); // images 有 1 个设置项 → 角标
    assert.ok(screen.getByRole('slider', { name: '主题时段' }), '门户页里有外观设置');

    fireEvent.click(screen.getByText('qq'));
    assert.ok(screen.getByRole('heading', { name: 'qq' }));
    assert.ok(screen.getByText(/这个服务没有可改的设置项/));

    fireEvent.click(screen.getByText('images'));
    assert.ok(screen.getByRole('heading', { name: 'images' }));
    assert.ok(screen.getByText('max_mb'));
  });

  it('设置默认页：名册先到也回门户；重新进设置仍回门户', async () => {
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    stubFetch((url) => {
      if (url === '/api/overview') {
        return gate.then(() => jsonResponse(okOverview(rosterOf(['approval', 'images'], 1))));
      }
      return undefined;
    });
    installFakeEventSource();
    render(<App />);
    fireEvent.click(screen.getByRole('button', { name: '设置' }));

    // 名册先到、overview.self 还没回来：临时退回第一个服务。
    await emitSSE('roster', rosterOf(['approval', 'images'], 2));
    assert.ok(await screen.findByRole('heading', { name: 'approval' }));

    // overview 回来后知道门户是谁，默认页必须切回门户。
    await act(async () => {
      release();
      await Promise.resolve();
    });
    assert.ok(await screen.findByRole('heading', { name: 'frontend' }), '默认页应为门户');
    assert.ok(screen.getByRole('slider', { name: '主题时段' }));

    // 手动点过别的服务 → 关闭再进设置，仍回到门户默认页。
    fireEvent.click(screen.getByText('images'));
    assert.ok(screen.getByRole('heading', { name: 'images' }));
    fireEvent.click(screen.getByRole('button', { name: '关闭设置' }));
    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    assert.ok(await screen.findByRole('heading', { name: 'frontend' }));
  });

  it('写入：受理后待确认 → applied 事件 → 已生效 toast + 状态回落 + 重拉视图', async () => {
    const { calls } = await boot();
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('2');
    await screen.findByText('已写入 images.max_mb（待服务确认）');
    assert.ok(screen.getByText('待服务确认'));

    await emitSSE('settings', {
      cause: 'set_setting',
      service: 'images',
      results: [{ key: 'max_mb', value: '2', ok: true }],
    });
    await screen.findByText('已生效 images.max_mb = 2');
    await waitFor(() => assert.ok(screen.getByText('已改过'))); // 待确认清除，覆盖态保留
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2)); // 事件触发重拉
    expect(calls.some((c) => c.url === '/api/settings/set' && c.init?.method === 'POST')).toBe(true);
  });

  it('服务自报事件（report_setting）不冒充推送回执：待确认状态不被动，视图照旧重拉', async () => {
    const { calls } = await boot();
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('2');
    await screen.findByText('已写入 images.max_mb（待服务确认）');

    await emitSSE('settings', {
      cause: 'report_setting',
      service: 'images',
      results: [{ key: 'max_mb', ok: true }],
    });
    // 不 toast、不清状态：那一条不是我们这次推送的回执
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2));
    assert.ok(screen.getByText('待服务确认'));
    assert.equal(screen.queryByText(/已生效 images\.max_mb/), null);
  });

  it('失败事件：未送达 + 原因（行状态与 toast）', async () => {
    await boot();
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('2');
    await screen.findByText('已写入 images.max_mb（待服务确认）');
    await emitSSE('settings', {
      cause: 'set_setting',
      service: 'images',
      results: [{ key: 'max_mb', ok: false, error: '未送达: 拨号失败' }],
    });

    await screen.findByText('未生效 images.max_mb：未送达: 拨号失败');
    const failed = screen.getByText('未送达');
    assert.equal(failed.getAttribute('title'), '未送达: 拨号失败');
  });

  it('写入请求被中心拒绝：写入失败 toast，编辑态保留', async () => {
    await boot({ setResponse: () => jsonResponse({ error: '设置项未声明' }, 400) });
    await screen.findByRole('heading', { name: 'images' });

    writeNewValue('9');
    await screen.findByText('写入失败：设置项未声明');
    assert.ok(screen.getByLabelText('max_mb 修改')); // 仍在编辑，用户可修正
    assert.ok(screen.getByText('已改过')); // pending 已清除
  });

  it('roster revision 变化：名册更新并触发设置视图重拉', async () => {
    const { calls, setRoster } = await boot();
    await screen.findByRole('heading', { name: 'images' });

    const r2 = rosterOf(['images', 'qq', 'memory'], 2);
    setRoster(r2); // 服务端侧名册已推进（重拉 overview 应见到新名册）
    await emitSSE('roster', r2);
    await screen.findByText('服务 · 4');
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2));
  });

  it('名册竞态：SSE 新名册先到、首屏 overview 的旧响应后到 → 旧名册不得覆盖，守卫水位也不得倒退', async () => {
    // 场景：首屏 fetch 被"网络延迟"扣住（gate），期间 SSE 推来 rev 2 的新名册；
    // 随后 fetch 才带着 rev 1 的旧名册返回。旧代码会无条件 setRoster → 界面回退到两个服务，
    // 并把修订水位倒退回 1（于是下一条 rev 2 事件会再触发一次多余重拉）。
    let release!: () => void;
    const gate = new Promise<void>((r) => {
      release = r;
    });
    const calls = stubFetch((url) => {
      if (url === '/api/overview') {
        return gate.then(() => jsonResponse(okOverview(rosterOf(['images', 'qq'], 1))));
      }
      return undefined;
    });
    installFakeEventSource();
    render(<App />);
    fireEvent.click(screen.getByRole('button', { name: '设置' })); // 服务计数在设置页（门户视图没有它）

    // SSE 先到：rev 2，三个服务（此时 overview 还没回来，门户自身还没补进列表）。
    await emitSSE('roster', rosterOf(['images', 'qq', 'memory'], 2));
    await screen.findByText('服务 · 3');

    // 旧响应此时才回来（rev 1，两个服务）——必须被丢弃；门户自身补进列表后变为 4。
    await act(async () => {
      release();
      await Promise.resolve();
    });
    assert.ok(await screen.findByText('服务 · 4'), '更旧 revision 的名册不得覆盖新名册');

    // 守卫水位未倒退：再次收到 rev 2（与当前相同）→ 不触发多余重拉。
    await emitSSE('roster', rosterOf(['images', 'qq', 'memory'], 2));
    assert.equal(countCalls(calls, '/api/overview'), 2, '相同 revision 不应触发重拉');
  });

  it('待审命令：approvals 事件渲染卡片，点「批准执行」把裁决 POST 给 approval 通道', async () => {
    const calls: string[] = [];
    const posts: { url: string; body: unknown }[] = [];
    const base = stubFetch((url) => {
      if (url === '/api/overview') return jsonResponse(okOverview(rosterOf(['images', 'qq'])));
      return undefined;
    });
    installFakeEventSource();
    // 包一层：记录 /api/approvals 的 POST 与请求体（stubFetch 只管 GET 路由）。
    const origFetch = globalThis.fetch;
    globalThis.fetch = ((input: RequestInfo | URL, init?: RequestInit) => {
      const url = String(input);
      calls.push(url);
      if (url === '/api/approvals' && init?.method === 'POST') {
        posts.push({ url, body: JSON.parse(String(init.body)) });
        return Promise.resolve(jsonResponse({ ok: true, itemId: 'item-42' }));
      }
      return origFetch(input, init);
    }) as typeof fetch;
    render(<App />);

    await emitSSE('approvals', [
      {
        itemId: 'item-42',
        command: 'Get-Process',
        workDir: 'C:\\\\root',
        requestedBy: 'shell',
        reasonCode: 'NO_MATCHING_RULE',
        summary: '没有覆盖该命令的允许规则',
        unknowns: [],
        ruleIds: [],
        createdMs: 1,
        expiresMs: 2,
      },
    ]);

    await screen.findByText('待审命令 · 1（命令不会执行，直到你点批准）');
    assert.ok(screen.getByText('Get-Process'), '命令原文必须给用户看');
    assert.ok(screen.getByText(/NO_MATCHING_RULE/));

    fireEvent.click(screen.getByRole('button', { name: '批准执行' }));
    await waitFor(() => expect(posts.length).toBe(1));
    assert.deepEqual(posts[0]?.body, { item_id: 'item-42', decision: 'allow', note: '' });
    globalThis.fetch = origFetch;
    void base;
  });

  it('status 事件：连接状态切换；重连（false→true）触发重拉', async () => {
    const { calls } = await boot();
    await screen.findByRole('heading', { name: 'images' });

    await emitSSE('status', { connected: false });
    await screen.findByRole('img', { name: '未连接设置中心' });

    await emitSSE('status', { connected: true });
    await screen.findByRole('img', { name: '已连接设置中心' });
    await waitFor(() => expect(countCalls(calls, '/api/overview')).toBeGreaterThanOrEqual(2));
  });

  it('toast 4200ms 后自动消失（假时钟；不用 waitFor——其内部靠真实 interval 检测假时钟）', async () => {
    await boot({ setResponse: () => jsonResponse({ error: '设置项未声明' }, 400) });
    vi.useFakeTimers();
    try {
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10); // 冲掉首屏加载的微任务
      });
      assert.ok(screen.getByRole('heading', { name: 'images' }));

      writeNewValue('9');
      await act(async () => {
        await vi.advanceTimersByTimeAsync(10); // 冲掉保存失败与 toast 入列
      });
      assert.ok(screen.getByText('写入失败：设置项未声明'));

      await act(async () => {
        await vi.advanceTimersByTimeAsync(4300); // 超过 4200ms 自动消失窗口
      });
      assert.equal(screen.queryByText('写入失败：设置项未声明'), null);
    } finally {
      vi.useRealTimers();
    }
  });

  it('对话：首屏在对话视图，设置从输入框进入并可关闭', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });
    assert.equal(screen.queryByRole('region', { name: '设置' }), null, '首屏不该直接是设置页');

    fireEvent.click(screen.getByRole('button', { name: '设置' }));
    assert.ok(screen.getByRole('heading', { name: '设置' }));

    fireEvent.click(screen.getByRole('button', { name: '关闭设置' }));
    assert.ok(screen.getByRole('region', { name: '对话' }));
    assert.equal(screen.queryByRole('region', { name: '设置' }), null);
  });

  it('设置：独占 agent 面板的位置，左边栏换成服务名导览', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });
    assert.ok(screen.getByRole('navigation', { name: '会话与任务' }));
    assert.equal(screen.queryByRole('navigation', { name: '服务列表' }), null);

    fireEvent.click(screen.getByRole('button', { name: '设置' }));

    // 不是浮层：对话视图整体让位，设置正文住进 agent 面板的位置
    assert.equal(screen.queryByRole('region', { name: '对话' }), null, '设置不是抽屉，是把面板内容换掉');
    assert.ok(screen.getByRole('region', { name: '设置' }));
    // 左边栏改放服务名（字标、连接点、收起按钮都还在）
    assert.ok(screen.getByRole('navigation', { name: '服务列表' }));
    assert.equal(screen.queryByRole('navigation', { name: '会话与任务' }), null);
    assert.ok(await screen.findByText('服务 · 3'));
    assert.ok(screen.getByText('SUMP'));
    assert.ok(screen.getByRole('button', { name: '收起侧栏' }));
    assert.ok(await screen.findByRole('heading', { name: 'frontend' }), '门户排第一并默认选中');
    assert.ok(await screen.findByRole('slider', { name: '主题时段' }), '门户自己的外观设置');
    fireEvent.click(screen.getByText('images'));
    assert.ok(screen.getByText('max_mb'), '设置正文照旧渲染选中服务的设置项');
  });

  it('设置：Esc 关闭', async () => {
    await boot();
    await screen.findByRole('heading', { name: '设置' });

    fireEvent.keyDown(window, { key: 'Escape' });
    await waitFor(() => assert.equal(screen.queryByRole('region', { name: '设置' }), null));
  });

  it('设置：编辑态按 Esc 只退出编辑，不关闭设置页', async () => {
    await boot();
    fireEvent.click(screen.getAllByRole('button', { name: '修改' })[0]!);
    const input = screen.getByLabelText('max_mb 修改');

    fireEvent.keyDown(input, { key: 'Escape' });

    await waitFor(() => assert.equal(screen.queryByLabelText('max_mb 修改'), null));
    assert.ok(screen.getByRole('heading', { name: '设置' }), '设置页应仍然打开');
  });

  it('侧栏：收起是向上塌成只剩字标的一小块，点块即可展开', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });
    assert.ok(screen.getByRole('navigation', { name: '会话与任务' }));
    const brand = screen.getByText('SUMP');

    fireEvent.click(screen.getByRole('button', { name: '收起侧栏' }));
    assert.equal(screen.queryByRole('navigation', { name: '会话与任务' }), null, '收起后会话列表不该还占着地方');
    assert.equal(document.querySelector('.app')?.getAttribute('data-collapsed'), '1');
    assert.equal(screen.getByText('SUMP'), brand, '字标是同一个节点：收起不许重建它');
    assert.equal(screen.queryByRole('img', { name: '已连接设置中心' }), null, '收起态只剩字标，连接点也收起来');

    fireEvent.click(screen.getByRole('button', { name: '展开侧栏' }));
    assert.ok(screen.getByRole('navigation', { name: '会话与任务' }));
    assert.equal(document.querySelector('.app')?.getAttribute('data-collapsed'), null);
    assert.equal(screen.getByText('SUMP'), brand, '展开回来还是同一个字标节点');
  });

  it('对话：启动时向服务要历史回填；同一条重复推送不画两遍', async () => {
    const { calls } = await boot({
      openSettings: false,
      history: [
        { role: 'user', content: '早先的问题', at: '2026-10-17T10:00:00Z' },
        { role: 'assistant', content: '早先的答复', at: '2026-10-17T10:00:05Z' },
      ],
    });
    await screen.findByRole('region', { name: '对话' });
    await screen.findByText('早先的问题');
    assert.ok(screen.getByText('早先的答复'));
    assert.equal(
      screen.queryByText('发一条消息开始。回复会经 deliver 实时送回来。'),
      null,
      '有历史就不该再显示空态',
    );
    assert.ok(calls.some((c) => c.url.startsWith('/api/history')), '启动应拉一次历史');

    // 同一条答复又被 deliver 推一遍（历史尾巴与实时推送重叠）→ 只画一次
    await emitSSE('deliver', { text: '早先的答复', conversationId: 'web:private:owner', at: '2026-10-17T10:00:05Z' });
    assert.equal(screen.getAllByText('早先的答复').length, 1);
  });

  it('对话：历史取不到只提示，不阻断对话', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });
    // 默认桩已改过 /api/history（空列表）：不该出现任何错误提示
    assert.equal(screen.queryByRole('alert'), null);
  });

  it('对话：deliver 事件把智能体的消息落进对话', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });

    await emitSSE('deliver', { text: '任务完成', conversationId: 'web:private:owner', at: '2026-10-16T08:00:00Z' });
    await screen.findByText('任务完成');
  });

  it('对话：答复按 Markdown 渲染（代码块 / 列表 / 行内代码）', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });

    const md = ['结论如下：', '', '```ts', 'const a = 1;', '```', '', '- 第一条', '- 第二条', '', '执行 `npm test` 即可。'].join('\n');
    await emitSSE('deliver', { text: md, conversationId: 'web:private:owner', at: '2026-10-17T10:00:00Z' });

    await screen.findByText('const a = 1;');
    assert.ok(document.querySelector('.md pre code'), '代码块应渲染成 pre > code');
    assert.equal(document.querySelectorAll('.md li').length, 2);
    assert.equal(document.querySelector('.md')?.textContent?.includes('```'), false, '不该把围栏原样印出来');
    assert.ok(document.querySelector('.md p code')?.textContent === 'npm test');
  });

  it('任务：会话下面按任务展开，点任务跳回那一段；面板仍是一整段会话', async () => {
    await boot({
      openSettings: false,
      tasks: [
        { id: 'task-done', name: '上一次的活', status: 'done', updated_at: '2026-10-17T10:00:00Z' },
        { id: 'task-live', name: '正在做的活', status: 'active', updated_at: '2026-10-17T11:00:00Z' },
      ],
      history: [
        { role: 'user', content: '先把上次那件事收尾', at: '2026-10-17T09:59:00Z', task_id: 'task-done' },
        { role: 'assistant', content: '上次的答复', at: '2026-10-17T10:00:00Z', task_id: 'task-done' },
        { role: 'user', content: '现在做这个', at: '2026-10-17T11:00:00Z', task_id: 'task-live' },
      ],
    });
    await screen.findByRole('region', { name: '对话' });
    await screen.findByText('上次的答复');

    // 面板：整段会话都在，两次任务各起一个分节头
    assert.ok(screen.getByText('现在做这个'), '进行中任务的消息也在同一段会话里');
    assert.equal(document.querySelectorAll('.task-sep').length, 2);
    assert.deepEqual(
      Array.from(document.querySelectorAll('.task-sep-name')).map((e) => e.textContent),
      ['上一次的活', '正在做的活'],
    );
    assert.deepEqual(
      Array.from(document.querySelectorAll('.task-sep-state')).map((e) => e.textContent),
      ['已完成', '进行中'],
    );

    // 完结的任务不再是"只读视图"：这是会话，不是历史快照
    const box = screen.getByLabelText('消息') as HTMLTextAreaElement;
    assert.equal(box.disabled, false);
    // 未聚焦不显示提示：聚焦后才出现「说点什么…」
    assert.equal(box.getAttribute('placeholder'), '');
    fireEvent.focus(box);
    assert.equal(box.getAttribute('placeholder'), '说点什么…（Enter 发送，Shift+Enter 换行）');

    // 侧栏点任务 = 跳回那一段（jsdom 里没有布局，只验"落点标记"）
    fireEvent.click(screen.getByRole('button', { name: /上一次的活/ }));
    assert.ok(document.getElementById('task-task-done')?.classList.contains('is-jump'), '跳转要标出落点');

    // 会话节点可以收起来
    fireEvent.click(screen.getByRole('button', { name: /当前会话/ }));
    assert.equal(screen.queryByRole('button', { name: /上一次的活/ }), null, '收起后任务行收起');
    fireEvent.click(screen.getByRole('button', { name: /当前会话/ }));
    assert.ok(screen.getByRole('button', { name: /上一次的活/ }), '再点展开回来');
  });

  it('对话：progress 事件把思考 / 终端命令 / 终端输出画成轨迹条目', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });

    for (const p of [
      { seq: 2, kind: 'reasoning', content: '先看看现在几点，再决定要不要定闹钟。', meta: undefined },
      { seq: 3, kind: 'tool_call', content: 'Get-Date', meta: '{"tool":"shell__run","call_id":"c1"}' },
      { seq: 4, kind: 'tool_result', content: '2026-10-17 10:00:02\n第二行', meta: '{"tool":"shell__run","call_id":"c1"}' },
    ]) {
      await emitSSE('progress', {
        conversationId: 'web:private:owner',
        seq: p.seq,
        kind: p.kind,
        content: p.content,
        meta: p.meta,
        at: '2026-10-17T10:00:0' + String(p.seq) + 'Z',
      });
    }

    // 思考与输出默认折叠：摘要在外面，正文在 details 里（折叠时 open=false）
    assert.ok(await screen.findByText('深度思考'));
    assert.equal(document.querySelector('.trace-text')?.textContent, '先看看现在几点，再决定要不要定闹钟。');
    // 命令同时出现在对话轨迹与右侧消息卡片里：断言只针对会话正文。
    const conv = within(document.querySelector('.portal-list') as HTMLElement);
    assert.ok(conv.getByText('Get-Date'));
    assert.equal(conv.getAllByText('shell__run').length, 2, '命令与输出各带一次工具名');
    const output = document.querySelector('.step-result details') as HTMLDetailsElement;
    assert.equal(output.open, false, '输出默认折叠');
    assert.equal(output.querySelector('.trace-pre')?.textContent, '2026-10-17 10:00:02\n第二行');
    assert.match(output.querySelector('summary')?.textContent ?? '', /2 行/);
    // 每一步都有自己的图标（时间线靠它定位，图标之间由 CSS 连线）
    assert.equal(document.querySelectorAll('.step-mark svg').length, 3);

    // 同一 seq 再推一遍（重连/补发）→ 不画第二遍（只数会话正文，右侧卡片另有一份）
    await emitSSE('progress', {
      conversationId: 'web:private:owner',
      seq: 3,
      kind: 'tool_call',
      content: 'Get-Date',
      meta: '{"tool":"shell__run","call_id":"c1"}',
      at: '2026-10-17T10:00:02Z',
    });
    assert.equal(within(document.querySelector('.portal-list') as HTMLElement).getAllByText('Get-Date').length, 1);
  });
  it('对话：发送 → POST /api/message + 本地回显', async () => {
    const { calls } = await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });

    fireEvent.change(screen.getByLabelText('消息'), { target: { value: '你好' } });
    fireEvent.click(screen.getByRole('button', { name: '发送' }));

    await screen.findByText('你好'); // 本地回显（受理回执不代表已有回复）
    await waitFor(() => expect(calls.some((c) => c.url === '/api/message' && c.init?.method === 'POST')).toBe(true));
    assert.equal(screen.queryByRole('alert'), null, '受理成功不应留下错误提示');
  });

  it('任务：进行中但当前窗口没记录的点一下就恢复，并把这一段记录载入会话', async () => {
    const { calls } = await boot({
      openSettings: false,
      tasks: [{ id: 'task-live', name: '正在做的活', status: 'active', updated_at: '2026-10-17T11:00:00Z' }],
      history: [],
    });
    await screen.findByRole('region', { name: '对话' });
    await screen.findByRole('button', { name: /正在做的活/ });

    fireEvent.click(screen.getByRole('button', { name: /正在做的活/ }));

    await screen.findByText('恢复出来的记录');
    assert.equal(countCalls(calls, '/api/tasks/resume'), 1, '应调用一次恢复');
    assert.equal(document.querySelectorAll('.task-sep').length, 1);
  });

  it('当前任务是最近活跃的那个：多条进行中任务时按 updated_at 判，而不是最早创建的', async () => {
    await boot({
      openSettings: false,
      tasks: [
        { id: 'task-old', name: '老的活', status: 'active', updated_at: '2026-10-17T09:00:00Z' },
        { id: 'task-new', name: '新的活', status: 'active', updated_at: '2026-10-17T12:00:00Z' },
      ],
      // 两条都已有记录，侧栏就是可跳状态，标题里才会带「当前任务」。
      history: [
        { role: 'user', content: '老的活的问题', at: '2026-10-17T09:00:00Z', task_id: 'task-old' },
        { role: 'user', content: '新的活的问题', at: '2026-10-17T12:00:00Z', task_id: 'task-new' },
      ],
    });
    await screen.findByRole('region', { name: '对话' });
    await screen.findByText('新的活的问题');

    assert.ok(screen.getByTitle('新的活 · 当前任务'), '最近活跃的进行中任务才是当前任务');
    assert.equal(screen.queryByTitle('老的活 · 当前任务'), null);
  });

  it('对话：发送未被受理 → 明确提示未送达，草稿保留、回显撤掉', async () => {
    await boot({ openSettings: false, messageResponse: () => jsonResponse({ error: '未知目标服务: reasoner（不在名册里）' }, 503) });
    await screen.findByRole('region', { name: '对话' });

    fireEvent.change(screen.getByLabelText('消息'), { target: { value: '你好' } });
    fireEvent.click(screen.getByRole('button', { name: '发送' }));

    await screen.findByText(/未送达：未知目标服务: reasoner/);
    const box = screen.getByLabelText('消息') as HTMLTextAreaElement;
    assert.equal(box.value, '你好', '未受理时草稿要留给用户重试');
    // 只看消息流：textarea 的受控值本身就是它的 textContent，不能拿全页 queryByText 判断。
    const list = document.querySelector('.portal-list');
    assert.ok(list !== null);
    assert.equal(list.textContent?.includes('你好') ?? false, false, '未受理的消息不该留在对话里');
  });

  it('对话：首屏历史与实时投递竞态——加载期间到达的 deliver 不被历史覆盖', async () => {
    let resolveHistory!: (r: Response) => void;
    const historyResponse = new Promise<Response>((resolve) => {
      resolveHistory = resolve;
    });
    stubFetch((url) => {
      if (url === '/api/overview') return jsonResponse(okOverview(rosterOf(['images', 'qq'])));
      if (url === '/api/conversations') return jsonResponse({ conversations: [PORTAL_CONVERSATION()] });
      if (url.startsWith('/api/tasks')) return jsonResponse({ conversationId: 'web:private:owner', tasks: [] });
      if (url.startsWith('/api/history')) return historyResponse;
      return undefined;
    });
    installFakeEventSource();
    render(<App />);
    await screen.findByRole('region', { name: '对话' });

    await emitSSE('deliver', { text: '先到的答复', taskId: 'task-9', at: '2026-10-17T10:00:01Z' });
    await screen.findByText('先到的答复');

    resolveHistory(
      jsonResponse({
        conversationId: 'web:private:owner',
        messages: [
          { role: 'user', content: '历史里的提问', at: '2026-10-17T09:59:00Z', seq: 1, task_id: 'task-8' },
          { role: 'assistant', content: '历史里的答复', at: '2026-10-17T09:59:30Z', seq: 2, task_id: 'task-8' },
        ],
      }),
    );

    await screen.findByText('历史里的答复');
    assert.ok(screen.getByText('先到的答复'), '实时消息不能被历史覆盖');
  });

  it('对话：输入区是浮层（消息流在它背后穿过），不是坐在流里的占位', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });

    const dock = document.querySelector('.dock');
    assert.ok(dock, '输入区必须挂在一个浮层容器里');
    assert.ok(dock.querySelector('.composer'), 'composer 应该在浮层内');
    assert.equal(dock.contains(document.querySelector('.portal-list')), false, '消息流不能被塞进浮层');
  });

  it('外观：拖动主题时段会切换整站深浅、整体底色与强调色，并记住偏好；开关可恢复跟随', async () => {
    window.localStorage.clear();
    await boot({ select: null }); // 门户排第一并默认选中，外观设置就在门户页里

    const slider = screen.getByRole('slider', { name: '主题时段' }) as HTMLInputElement;
    const follow = screen.getByRole('switch', { name: '跟随时间' });
    assert.equal(follow.getAttribute('aria-checked'), 'true');

    fireEvent.change(slider, { target: { value: '12' } });

    assert.equal(follow.getAttribute('aria-checked'), 'false');
    assert.equal(slider.value, '12');
    assert.equal(document.documentElement.dataset.theme, 'light');
    assert.equal(document.documentElement.style.getPropertyValue('--bg-0'), '#FBF7EA');
    assert.equal(document.documentElement.style.getPropertyValue('--accent'), '#B45309');
    assert.deepEqual(JSON.parse(window.localStorage.getItem('sump.portal.background') ?? 'null'), {
      auto: false,
      hour: 12,
    });

    fireEvent.change(slider, { target: { value: '22' } });
    assert.equal(document.documentElement.dataset.theme, 'dark');
    assert.equal(document.documentElement.style.getPropertyValue('--bg-0'), '#0A0C14');
    assert.equal(document.documentElement.style.getPropertyValue('--accent'), '#A78BFA');

    fireEvent.click(follow);
    assert.equal(follow.getAttribute('aria-checked'), 'true');
    assert.equal(JSON.parse(window.localStorage.getItem('sump.portal.background') ?? '{}').auto, true);
  });

  it('对话：timer 快照画成倒计时；到点显示「到点…」，空快照即消失', async () => {
    await boot({ openSettings: false });
    await screen.findByRole('region', { name: '对话' });

    const soon = new Date(Date.now() + 90_000).toISOString();
    await emitSSE('timers', [{ itemId: 'job-1', caller: 'reasoner', ms: 90000, fireAt: soon, note: '等构建' }]);
    // 倒计时同时出现在输入框上方的计时条与右侧消息卡片：断言只针对计时条。
    const strip = within(document.querySelector('.timer-strip') as HTMLElement);
    await strip.findByText(/\d{2}:\d{2} 后唤醒/);
    assert.ok(strip.getByText('等构建'), '备注要显示出来');

    // 到点（fire_at 已过去）但快照还没更新 → 显示「到点…」，不假装没有计时。
    // 计时条与消息卡片都会显示「到点…」，这里只要求至少出现一处。
    await emitSSE('timers', [
      { itemId: 'job-1', caller: 'reasoner', ms: 1000, fireAt: new Date(Date.now() - 1000).toISOString(), note: '' },
    ]);
    await waitFor(() => assert.ok(screen.queryAllByText('到点…').length >= 1));

    // timer 推来不含它的快照 → 倒计时消失（卡片随快照一起移除）。
    await emitSSE('timers', []);
    await waitFor(() => assert.equal(screen.queryAllByText('到点…').length, 0));
  });

  it('倒计时读数格式：不到一小时 mm:ss，超过带小时，已过归零', () => {
    assert.equal(remainingText(90_000), '01:30');
    assert.equal(remainingText(3_600_000), '1:00:00');
    assert.equal(remainingText(-5), '00:00');
  });
});
