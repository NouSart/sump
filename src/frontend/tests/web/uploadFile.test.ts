// 浏览器侧的 uploadFile：把 File 读成**不带 data: 前缀**的标准 base64，再以 JSON 发出去。
// 这条路径在组件测试里被跳过了（那时直接注入结果给 onUpload），而它恰恰是"文件真的从磁盘读出来"
// 的那一步——漏测就等于假设浏览器行为，所以单独钉一次。
import assert from 'node:assert/strict';
import { afterEach, describe, it, vi } from 'vitest';
import { uploadFile } from '../../src/web/api';

interface Captured {
  url: string;
  body: { filename?: string; mime?: string; data?: string };
}

function stubFetch(reply: unknown, captured: Captured[]): void {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (url: string, init?: RequestInit) => {
      captured.push({ url, body: JSON.parse(String(init?.body)) as Captured['body'] });
      return new Response(JSON.stringify(reply), {
        status: 200,
        headers: { 'content-type': 'application/json' },
      });
    }),
  );
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe('uploadFile', () => {
  it('读成裸 base64（去掉 data: 前缀）并带上文件名与 mime', async () => {
    const captured: Captured[] = [];
    stubFetch({ kind: 'file', id: 'f1', filename: 'a.txt', mime: 'text/plain', size: 2 }, captured);

    const out = await uploadFile(new File(['hi'], 'a.txt', { type: 'text/plain' }));
    assert.equal(out.id, 'f1');
    assert.equal(captured.length, 1);
    assert.equal(captured[0]?.url, '/api/upload');
    assert.equal(captured[0]?.body.filename, 'a.txt');
    assert.equal(captured[0]?.body.mime, 'text/plain');
    // 'hi' 的 base64 是 'aGk='：带上前缀会被门户按"不是标准 base64"拒掉。
    assert.equal(captured[0]?.body.data, 'aGk=');
  });

  it('空文件也原样发（是否允许由服务侧决定，前端不猜）', async () => {
    const captured: Captured[] = [];
    stubFetch({ kind: 'file', id: 'f2', filename: 'e.txt', mime: '', size: 0 }, captured);
    await uploadFile(new File([], 'e.txt', { type: '' }));
    assert.equal(captured[0]?.body.data, '');
    assert.equal(captured[0]?.body.mime, '');
  });

  it('服务侧拒绝时抛出它的原话（不吞成 HTTP 码）', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(async () =>
        new Response(JSON.stringify({ error: '文件太大：门户上限 20.0 MB' }), {
          status: 413,
          headers: { 'content-type': 'application/json' },
        }),
      ),
    );
    await assert.rejects(
      () => uploadFile(new File(['x'], 'big.bin', { type: 'application/octet-stream' })),
      /门户上限 20\.0 MB/,
    );
  });
});
