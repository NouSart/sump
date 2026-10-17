// 前端服务的 WS 端点：接受其他服务的 jump。
//
// 门户对外只提供 deliver（智能体 → 用户）：投递交给 DeliverHub，由 SSE 推给浏览器。
// 回执只表示"已送达面板"，**不表示用户看过**——与 shell 的"受理即回执"同一取向。
// 其余动作一律回执 unknown action，保证注册进名册的地址真实可达、行为诚实。
import type { Server as HttpServer } from 'node:http';
import { WebSocketServer, type WebSocket } from 'ws';
import type { RawData } from 'ws';
import { PORTAL_CONVERSATION_ID, checkRequestTrust, parsePendingApproval, type DeliverMessage, type PendingApprovalItem, type TimerItem, type TimerState, type TrustConfig } from './portal.ts';
import { type Provide, type Setting, TypeJump, decodeEnvelope, encodeEnvelope, frameBytes, newResponse } from './protocol.ts';

// portalProvides 是注册时上报的动作清单——名册里因此看得见门户能收 deliver、计时快照与待审命令。
export const portalProvides: Provide[] = [
  { action: 'deliver', input: '{text, conversation_id}', output: '回执（已送达面板）' },
  { action: 'timer_state', input: '{boss?, seq, timers[]}', output: '回执（已登记展示；accepted 说明这条快照有没有被采纳）' },
  { action: 'approval_pending', input: '{item_id, command, work_dir, reason_code, summary, expires_ms}', output: '回执（已登记待审；用户裁决经 approval_decide 送回 approval）' },
];

// portalSettings 是门户声明的设置项：**全是只读项**——门户的参数是启动参数（改它要重启），
// 进面板只为看得见。门户不消费 configure（只读项中心也不会下发），所以这里不发明一套设置机制。
export function portalSettings(cfg: { name: string; addr: string; centerURL: string; reasoner: string; webDir?: string }): Setting[] {
  return [
    { key: 'name', default: cfg.name, readonly: true },
    { key: 'addr', default: cfg.addr, readonly: true },
    { key: 'center', default: cfg.centerURL, readonly: true },
    { key: 'reasoner', default: cfg.reasoner, readonly: true },
    { key: 'web', default: cfg.webDir ?? '', readonly: true },
  ];
}

// parseTimerItems 逐条校验 timer 推来的计时项：外部输入不可信，形状不对的单条丢弃
// （丢一条不影响整批——宁可少画一个倒计时，也不画一个错的）。
export function parseTimerItems(raw: unknown, log: (m: string) => void): TimerItem[] {
  if (!Array.isArray(raw)) return [];
  const out: TimerItem[] = [];
  for (const it of raw) {
    const o = it as { item_id?: unknown; caller?: unknown; ms?: unknown; fire_at?: unknown; note?: unknown };
    if (typeof o?.item_id !== 'string' || o.item_id === '' || typeof o.fire_at !== 'string' || typeof o.ms !== 'number') {
      log('timer_state 里有形状不符的计时项（丢弃一条）');
      continue;
    }
    out.push({
      itemId: o.item_id,
      caller: typeof o.caller === 'string' ? o.caller : '',
      ms: o.ms,
      fireAt: o.fire_at,
      note: typeof o.note === 'string' ? o.note : '',
    });
  }
  return out;
}

export function attachJumpEndpoint(
  server: HttpServer,
  path: string,
  name: string,
  log: (m: string) => void,
  onDeliver: (m: DeliverMessage) => void,
  onTimers: (s: TimerState) => boolean,
  onApproval: (item: PendingApprovalItem) => void,
  trust: TrustConfig,
): WebSocketServer {
  const wss = new WebSocketServer({ noServer: true });

  const handleConn = (ws: WebSocket): void => {
    ws.on('message', (data: RawData) => {
      let env;
      try {
        env = decodeEnvelope(frameBytes(data));
      } catch {
        log('WS 端点上收到坏帧（丢弃）');
        return;
      }
      if (env.type !== TypeJump) {
        ws.send(encodeEnvelope(newResponse(env, name, { ok: false, error: `unsupported message type: ${env.type}` })));
        return;
      }
      const jp = env.payload as { action?: string; input?: unknown } | undefined;
      if (jp?.action === 'deliver') {
        const input = (jp.input ?? {}) as { text?: unknown; conversation_id?: unknown };
        if (typeof input.text !== 'string' || input.text === '') {
          ws.send(encodeEnvelope(newResponse(env, name, { ok: false, error: 'deliver 需要非空 text' })));
          return;
        }
        const conversationId =
          typeof input.conversation_id === 'string' && input.conversation_id !== ''
            ? input.conversation_id
            : PORTAL_CONVERSATION_ID;
        onDeliver({ text: input.text, conversationId, at: new Date().toISOString() });
        ws.send(encodeEnvelope(newResponse(env, name, { ok: true, data: { delivered: true } })));
        return;
      }
      if (jp?.action === 'timer_state') {
        const input = (jp.input ?? {}) as { boss?: unknown; seq?: unknown; timers?: unknown };
        const accepted = onTimers({
          boss: typeof input.boss === 'string' ? input.boss : '',
          seq: typeof input.seq === 'number' ? input.seq : 0,
          items: parseTimerItems(input.timers, log),
        });
        ws.send(encodeEnvelope(newResponse(env, name, { ok: true, data: { accepted } })));
        return;
      }
      if (jp?.action === 'approval_pending') {
        const item = parsePendingApproval(jp.input, log);
        if (item !== null) onApproval(item);
        ws.send(encodeEnvelope(newResponse(env, name, { ok: true, data: { accepted: item !== null } })));
        return;
      }
      ws.send(encodeEnvelope(newResponse(env, name, { ok: false, error: `unknown action: ${jp?.action ?? ''}` })));
    });
  };

  server.on('upgrade', (req, socket, head) => {
    const url = new URL(req.url ?? '/', 'http://127.0.0.1');
    if (url.pathname !== path) {
      socket.destroy();
      return;
    }
    // 信任栅栏对 WS 同样生效：服务之间的连接不带 Origin（放行），而浏览器发起的
    // 握手必然带 Origin——否则恶意页面能直连这里往门户里塞假投递。
    const denied = checkRequestTrust(req.headers.host, req.headers.origin, trust);
    if (denied !== null) {
      log(`WS 端点拒绝连接: ${denied}`);
      socket.destroy();
      return;
    }
    wss.handleUpgrade(req, socket, head, (ws) => handleConn(ws));
  });

  return wss;
}
