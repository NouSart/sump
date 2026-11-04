// web/api.ts 的 HTTP 封装测试：URL / 方法 / 请求体；成功与三类失败（业务错误、
// 无 body 错误、非 JSON body）统一抛 Error。
import assert from 'node:assert/strict';
import { describe, it, expect } from 'vitest';
import { fetchOverview, resetSetting, setSetting } from '../../src/web/api';
import { jsonResponse, stubFetch } from './harness';

describe('web/api', () => {
  it('fetchOverview：GET /api/overview，成功返回解析后的 body', async () => {
    const calls = stubFetch((url) =>
      url === '/api/overview' ? jsonResponse({ connected: true, services: [] }) : undefined,
    );
    const body = await fetchOverview();
    assert.deepEqual(body, { connected: true, services: [] });
    assert.equal(calls.length, 1);
    assert.equal(calls[0]?.url, '/api/overview');
    assert.equal(calls[0]?.init, undefined);
  });

  it('setSetting：POST /api/settings/set，JSON body 含 service/key/value，返回 services', async () => {
    const calls = stubFetch((url, init) => {
      if (url !== '/api/settings/set') return undefined;
      assert.equal(init?.method, 'POST');
      assert.equal((init?.headers as Record<string, string>)['content-type'], 'application/json');
      assert.equal(init?.body, JSON.stringify({ service: 'images', key: 'max_mb', value: '2' }));
      return jsonResponse({ ok: true, services: [{ service: 'images', settings: [] }] });
    });
    const out = await setSetting('images', 'max_mb', '2');
    assert.equal(out.length, 1);
    assert.equal(out[0]?.service, 'images');
    assert.equal(calls.length, 1);
  });

  it('resetSetting：POST /api/settings/reset，body 只含 service/key；缺 services 时回落空数组', async () => {
    stubFetch((url) => (url === '/api/settings/reset' ? jsonResponse({ ok: true }) : undefined));
    const out = await resetSetting('images', 'max_mb');
    assert.deepEqual(out, []);
  });

  it('业务错误（非 2xx + {error}）：抛 Error，消息为远端原因', async () => {
    stubFetch(() => jsonResponse({ error: '设置项未声明' }, 400));
    await expect(fetchOverview()).rejects.toThrow('设置项未声明');
  });

  it('非 2xx 且 body 无 error：抛 HTTP 状态码消息', async () => {
    stubFetch(() => new Response('oops', { status: 502 }));
    await expect(fetchOverview()).rejects.toThrow('HTTP 502');
  });

  it('非 JSON body：不抛解析错误（body 视为空）', async () => {
    stubFetch(() => new Response('not json', { status: 200 }));
    const body = await fetchOverview();
    assert.equal(body, null);
  });
});
