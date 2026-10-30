// 传输与入口限制：事件流并发上限、WS 单帧上限、监听地址的可回连约束。
import assert from 'node:assert/strict';
import { mkdtemp, writeFile } from 'node:fs/promises';
import { request as httpRequest } from 'node:http';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import test from 'node:test';
import { WebSocket } from 'ws';
import { startFrontend, type FrontendConfig } from '../src/server/app.ts';
import { StubCenter } from './stub-center.ts';

const silent = (): void => {};

async function baseConfig(): Promise<FrontendConfig> {
  const dir = await mkdtemp(join(tmpdir(), 'sump-web-'));
  await writeFile(join(dir, 'index.html'), '<!doctype html><title>面板</title>', 'utf8');
  return {
    name: 'frontend',
    addr: '127.0.0.1:0',
    centerURL: '',
    webDir: dir,
    heartbeatMS: 50,
    reasoner: 'reasoner',
    memory: 'memory',
    localesDir: dir,
    lang: 'zh-CN',
    approval: 'approval',
    trustedHosts: [],
  };
}

// openEventStream 打开一条 SSE 连接：收到首块数据（或非 200 响应头）后返回。
function openEventStream(addr: string): Promise<{ status: number; close: () => void }> {
  const idx = addr.lastIndexOf(':');
  return new Promise((resolve, reject) => {
    const req = httpRequest(
      {
        host: addr.slice(0, idx),
        port: Number(addr.slice(idx + 1)),
        path: '/api/events',
        method: 'GET',
        headers: { host: addr },
      },
      (res) => {
        const close = (): void => {
          req.destroy();
          res.destroy();
        };
        if (res.statusCode !== 200) {
          res.resume();
          resolve({ status: res.statusCode ?? 0, close });
          return;
        }
        res.on('error', () => {});
        res.once('data', () => resolve({ status: 200, close }));
      },
    );
    req.on('error', reject);
    req.end();
  });
}

test('事件流：并发连接有上限，超出返回 503', async (t) => {
  const stub = new StubCenter();
  const cfg = await baseConfig();
  cfg.centerURL = await stub.start();
  const handle = await startFrontend(cfg, silent);
  const streams: { status: number; close: () => void }[] = [];
  t.after(async () => {
    for (const s of streams) s.close();
    await handle.close();
    await stub.stop();
  });

  for (let i = 0; i < 32; i++) streams.push(await openEventStream(handle.addr));
  streams.push(await openEventStream(handle.addr));

  assert.equal(streams[0]?.status, 200);
  assert.equal(streams.at(-1)?.status, 503, '第 33 条事件流应被容量拒绝');
});

test('WS：单帧超过 1 MiB 直接断开（不把进程带崩）', async (t) => {
  const stub = new StubCenter();
  const cfg = await baseConfig();
  cfg.centerURL = await stub.start();
  const handle = await startFrontend(cfg, silent);
  t.after(async () => {
    await handle.close();
    await stub.stop();
  });

  const ws = new WebSocket(`ws://${handle.addr}/ws`);
  await new Promise<void>((resolve, reject) => {
    ws.once('open', () => resolve());
    ws.once('error', reject);
  });
  const closed = new Promise<number>((resolve) => {
    ws.once('close', (code) => resolve(code));
  });
  ws.send(Buffer.alloc((1 << 20) + 1));
  assert.equal(await closed, 1009, '超限帧应按 1009（消息过大）关闭连接');
});

test('监听地址：拒绝通配绑定（名册里的 ws 地址必须可回连）', async () => {
  const cfg = await baseConfig();
  cfg.addr = '0.0.0.0:0';
  await assert.rejects(() => startFrontend(cfg, silent), /通配/);
});
