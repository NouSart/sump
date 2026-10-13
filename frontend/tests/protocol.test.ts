// 协议层测试：信封键名 / roundtrip，以及 Go 侧 protocol 包产出字节的解析（跨语言锚点）。
import assert from 'node:assert/strict';
import test from 'node:test';
import { decode } from '@msgpack/msgpack';
import { decodeEnvelope, encodeEnvelope, newEnvelope, newID } from '../src/server/protocol.ts';

test('newID：26 字符 ULID 形态且互不相同', () => {
  const a = newID();
  const b = newID();
  assert.equal(a.length, 26);
  assert.match(a, /^[0-9A-HJKMNP-TV-Z]{26}$/);
  assert.notEqual(a, b);
});

test('信封 roundtrip：键名与 Go 对齐、空字段不出现', () => {
  const env = newEnvelope('jump', 'svc-a', 'settings-center', '', {
    action: 'list_settings',
    input: undefined,
  });
  const bytes = encodeEnvelope(env);
  const raw = decode(bytes) as Record<string, unknown>;
  assert.deepEqual(Object.keys(raw).sort(), ['from', 'id', 'payload', 'to', 'trace', 'type', 'v']);
  assert.equal(raw.v, 1);
  assert.equal(raw.type, 'jump');
  assert.equal(raw.from, 'svc-a');
  assert.equal(raw.to, 'settings-center');

  const back = decodeEnvelope(bytes);
  assert.equal(back.id, env.id);
  assert.equal(back.trace, env.trace);
  const payload = back.payload as { action?: string };
  assert.equal(payload?.action, 'list_settings');

  // payload 是嵌套值（与 Go 的 RawMessage 语义一致），不是 bin 字节块。
  assert.equal(typeof raw.payload, 'object');
  assert.ok(!(raw.payload instanceof Uint8Array));

  const noPayload = encodeEnvelope(newEnvelope('heartbeat', 'a', 'b', 'T'));
  assert.equal('payload' in (decode(noPayload) as Record<string, unknown>), false);
});

// Go 侧 protocol 包生成的字节（由 protocol/tmpgen 生成后固化；ULID 形态 id 固定）。
const goRegister =
  'h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVnaXN0ZXKkZnJvbaVzdmMtYaJ0b69zZXR0aW5ncy1jZW50ZXKncGF5bG9hZIakbmFtZaVzdmMtYaRhZGRytndzOi8vMTI3LjAuMC4xOjk5OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhqHByb3ZpZGVzkYOmYWN0aW9upHBpbmemb3V0cHV0pHBvbmekdG9vbMOoc2V0dGluZ3ORgqNrZXmsZGVtby5zZXR0aW5np2RlZmF1bHSiNW2saGVhcnRiZWF0X21z0wAAAAAAADqY';
const goRoster =
  'h6F2AaJpZLowMUZJWFJPU1RFUjAwMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0ypHR5cGWmcm9zdGVypGZyb22vc2V0dGluZ3MtY2VudGVyonRvpXN2Yy1hp3BheWxvYWSCqHNlcnZpY2VzkYSkbmFtZaVzdmMtYaRhZGRytndzOi8vMTI3LjAuMC4xOjk5OTkvd3OrZGVzY3JpcHRpb26s5aS55YW35pyN5YqhrGhlYXJ0YmVhdF9tc9MAAAAAAAA6mKhyZXZpc2lvbtMAAAAAAAAAAw==';
const goResponse =
  'h6F2AaJpZLowMUZJWFJFR0lTVEVSMDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0xpHR5cGWocmVzcG9uc2WkZnJvba9zZXR0aW5ncy1jZW50ZXKidG+lc3ZjLWGncGF5bG9hZIKib2vDpGRhdGGBqHNlcnZpY2VzkYKnc2VydmljZaVzdmMtYahzZXR0aW5nc5GEo2tleaxkZW1vLnNldHRpbmeldmFsdWWjMTBtp2RlZmF1bHSiNW2qb3ZlcnJpZGRlbsM=';
const goHeartbeat =
  'h6F2AaJpZLowMUZJWEhFQVJUQkVBVDAwMDAwMDAwMDAwMKV0cmFjZadUUkFDRS0zpHR5cGWpaGVhcnRiZWF0pGZyb22lc3ZjLWGidG+vc2V0dGluZ3MtY2VudGVyp3BheWxvYWSBpnN0YXR1c6Jvaw==';

test('解析 Go 产出的 register 信封（字段逐项）', () => {
  const env = decodeEnvelope(Buffer.from(goRegister, 'base64'));
  assert.equal(env.v, 1);
  assert.equal(env.type, 'register');
  assert.equal(env.from, 'svc-a');
  assert.equal(env.to, 'settings-center');
  assert.equal(env.trace, 'TRACE-1');
  assert.ok(env.id.startsWith('01FIXREGISTER'), `id=${env.id}`);
  const p = env.payload as {
    name?: string;
    addr?: string;
    description?: string;
    provides?: { action?: string; output?: string; tool?: boolean }[];
    settings?: { key?: string; default?: string }[];
    heartbeat_ms?: number;
  };
  assert.equal(p?.name, 'svc-a');
  assert.equal(p?.addr, 'ws://127.0.0.1:9999/ws');
  assert.equal(p?.description, '夹具服务');
  assert.equal(p?.provides?.[0]?.action, 'ping');
  assert.equal(p?.provides?.[0]?.output, 'pong');
  assert.equal(p?.provides?.[0]?.tool, true);
  assert.equal(p?.settings?.[0]?.key, 'demo.setting');
  assert.equal(p?.settings?.[0]?.default, '5m');
  assert.equal(p?.heartbeat_ms, 15000);
});

test('解析 Go 产出的 roster 广播', () => {
  const env = decodeEnvelope(Buffer.from(goRoster, 'base64'));
  assert.equal(env.type, 'roster');
  assert.equal(env.from, 'settings-center');
  const r = env.payload as { services?: { name?: string; addr?: string; heartbeat_ms?: number }[]; revision?: number };
  assert.equal(r?.revision, 3);
  assert.equal(r?.services[0]?.name, 'svc-a');
  assert.equal(r?.services[0]?.addr, 'ws://127.0.0.1:9999/ws');
  assert.equal(r?.services[0]?.heartbeat_ms, 15000);
});

test('解析 Go 产出的 response（data 二次解码为 SettingsResult）', () => {
  const env = decodeEnvelope(Buffer.from(goResponse, 'base64'));
  assert.equal(env.type, 'response');
  assert.equal(env.from, 'settings-center');
  assert.equal(env.to, 'svc-a');
  assert.equal(env.trace, 'TRACE-1'); // NewResponse 沿用请求 trace
  const rp = env.payload as { ok?: boolean; data?: unknown };
  assert.equal(rp?.ok, true);
  const sr = rp?.data as {
    services?: { service?: string; settings?: { key?: string; value?: string; default?: string; overridden?: boolean }[] }[];
  };
  assert.equal(sr?.services[0]?.service, 'svc-a');
  assert.equal(sr?.services[0]?.settings[0]?.key, 'demo.setting');
  assert.equal(sr?.services[0]?.settings[0]?.value, '10m');
  assert.equal(sr?.services[0]?.settings[0]?.default, '5m');
  assert.equal(sr?.services[0]?.settings[0]?.overridden, true);
});

test('解析 Go 产出的 heartbeat 事件', () => {
  const env = decodeEnvelope(Buffer.from(goHeartbeat, 'base64'));
  assert.equal(env.type, 'heartbeat');
  assert.equal(env.from, 'svc-a');
  const hb = env.payload as { status?: string };
  assert.equal(hb?.status, 'ok');
});
