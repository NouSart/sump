// 门户（单用户）：会话身份、投递分发、浏览器信任栅栏。
//
// 定位：frontend 与 qq 是**同类型的用户交互门户**——对外提供 deliver（智能体 → 用户），
// 自己 jump user_message 给 reasoner（用户 → 智能体）。设置面板则是它的另一页。
//
// 单用户：固定一个会话 id（多用户以后再说），形状与 qq 的 qq:private:<uid> 一致。
export const PORTAL_CONVERSATION_ID = 'web:private:owner';

export interface DeliverMessage {
  text: string;
  conversationId: string;
  at: string;
}

// DeliverHub 把 WS 端点上收到的投递分发给所有 SSE 订阅者（浏览器可能开着多个标签页）。
export interface DeliverHub {
  publish(m: DeliverMessage): void;
  on(fn: (m: DeliverMessage) => void): () => void;
}

export function createDeliverHub(): DeliverHub {
  const listeners = new Set<(m: DeliverMessage) => void>();
  return {
    publish: (m) => {
      for (const fn of [...listeners]) fn(m);
    },
    on: (fn) => {
      listeners.add(fn);
      return () => listeners.delete(fn);
    },
  };
}

// ---------- 浏览器信任栅栏 ----------
//
// 为什么需要它：门户一旦能驱动智能体（而智能体手里有 shell），它就是一个"能执行命令的入口"。
// 只绑回环并不够——恶意页面可以把域名解析到 127.0.0.1（DNS rebinding）：浏览器看它是**同源**，
// 于是不发 CORS 预检，但 Host 头写着攻击者的域名。Host 是重绑伪造不了的那个头，所以栅栏钉它。
//
// 与 DSH 的 api-request-trust.ts 同一思路。注意：**它只证明"请求来自受信地址"，不建立身份**。

export interface TrustConfig {
  /** 实际绑定的 host:port，用于放行"用绑定地址直接访问"这一情形。 */
  boundAddr: string;
  /** 额外放行的 Host（`-trusted-hosts`，逗号分隔）。留给反代 / 局域网域名。 */
  trustedHosts: readonly string[];
}

/** isLoopbackHostname 判断主机名是否指向本机回环（与 DSH 的 loopback-hostname.ts 同判据）。 */
export function isLoopbackHostname(hostname: string): boolean {
  if (hostname === 'localhost' || hostname === '::1' || hostname === '[::1]') return true;
  const parts = hostname.split('.');
  return parts.length === 4
    && parts[0] === '127'
    && parts.every((p) => /^\d{1,3}$/.test(p) && Number(p) <= 255);
}

/** splitHostPort 拆 `host[:port]`；IPv6 字面量保留方括号。 */
function splitHostPort(value: string): { hostname: string; port: string } {
  if (value.startsWith('[')) {
    const end = value.indexOf(']');
    if (end < 0) return { hostname: value, port: '' };
    const rest = value.slice(end + 1);
    return { hostname: value.slice(0, end + 1), port: rest.startsWith(':') ? rest.slice(1) : '' };
  }
  const idx = value.lastIndexOf(':');
  // 无冒号，或冒号后不是端口（如裸 IPv6），整体当主机名。
  if (idx < 0 || !/^\d+$/.test(value.slice(idx + 1))) return { hostname: value, port: '' };
  return { hostname: value.slice(0, idx), port: value.slice(idx + 1) };
}

function isTrustedHost(host: string, bound: { hostname: string; port: string }, trusted: readonly string[]): boolean {
  const { hostname, port } = splitHostPort(host);
  if (trusted.includes(host) || trusted.includes(hostname)) return true;
  if (host === bound.hostname + (bound.port === '' ? '' : `:${bound.port}`)) return true;
  // 端口必须对上：不同端口是另一个服务，没有理由放行。
  if (port !== '' && port !== bound.port) return false;
  if (isLoopbackHostname(hostname)) return true;
  // 显式绑到非回环地址时，放行用该地址访问自身。
  return !isLoopbackHostname(bound.hostname) && hostname === bound.hostname;
}

/**
 * checkRequestTrust 校验一次 HTTP 请求是否来自受信地址。
 * @param hostHeader - 请求的 Host 头（浏览器必然带；重绑攻击伪造不了）。
 * @param originHeader - 请求的 Origin 头（跨站请求会带；同源读请求不带，故缺省不算错）。
 * @param cfg - 绑定地址与额外放行清单。
 * @returns 拒绝原因；通过则返回 null。
 */
export function checkRequestTrust(
  hostHeader: string | undefined,
  originHeader: string | undefined,
  cfg: TrustConfig,
): string | null {
  const bound = splitHostPort(cfg.boundAddr);
  if (hostHeader === undefined || hostHeader.trim() === '') return '缺少 Host 头';
  const host = hostHeader.trim().toLowerCase();
  if (!isTrustedHost(host, { hostname: bound.hostname.toLowerCase(), port: bound.port }, cfg.trustedHosts.map((h) => h.trim().toLowerCase()))) {
    return `Host 不在受信范围: ${host}（如确需从其他地址访问，用 -trusted-hosts 显式放行）`;
  }
  // 'null' 是沙箱 / file:// 页面的 Origin，同样不接受。
  if (originHeader !== undefined && originHeader !== 'null') {
    let originHost: string;
    try {
      originHost = new URL(originHeader).host.toLowerCase();
    } catch {
      return `非法 Origin: ${originHeader}`;
    }
    if (!isTrustedHost(originHost, { hostname: bound.hostname.toLowerCase(), port: bound.port }, cfg.trustedHosts.map((h) => h.trim().toLowerCase()))) {
      return `Origin 不在受信范围: ${originHeader}`;
    }
  }
  return null;
}
