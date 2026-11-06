// Package fetchsafe 提供「出站下载」的统一守卫（SSRF 防护）：images 与 files 服务都会
// 保存模型给的 URL，而模型可以编出 http://127.0.0.1:9000、http://169.254.169.254 这类
// 地址——没有守卫，就等于给模型开了一台能读遍本机所有服务端口与内网的读取原语。
//
// 守卫有四层，一层都不能少：
//  1. 协议白名单：只允许 http/https；file、gopher、http+unix 等一律拒绝；
//  2. 地址检查：解析主机名并逐个检查解析出的 IP——环回、私有网段（RFC1918/ULA）、
//     链路本地（含云_metadata_地址 169.254.169.254）、未指定（0.0.0.0/::）、组播全拒；
//     十进制（2130706433）/十六进制（0x7f000001）形态按名字解析不出来，同样落到拒绝；
//  3. 重定向每一跳都重新检查（CheckRedirect），且拨号时重新解析、把连接钉在检查过的 IP 上
//     （自定义 DialContext）——「检查」与「连接」之间换解析结果的 DNS rebinding 因此无效；
//  4. 错误指明对象（哪个 URL、哪个地址、什么理由），不静默放行。
//
// 关于测试逃生闸（Policy.AllowLoopback / 环境变量 SUMP_FETCH_ALLOW_LOOPBACK）：
// 本仓库的测试用 httptest 起对端，它们统统监听 127.0.0.1——没有逃生闸就测不出
// 「下载成功」这条路径。该闸只放行**首个地址**的环回，**重定向跳永远按最严格的标准检查**
// （否则公开的中转页就能借闸把下载带回本机服务），私有/链路本地等照旧拒绝。
// 默认关闭；它只能由测试或运维显式打开，**绝不能在生产默认开启**——那等于把
// 「读本机服务」的门重新交给模型。
package fetchsafe

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	// defaultTimeout 是一次下载的总超时（对齐 images/files 两个服务此前的行为）。
	defaultTimeout = 30 * time.Second
	// maxRedirects 限制重定向跳数（与 net/http 的默认行为一致，守卫层显式钉死）。
	maxRedirects = 10
	// dialTimeout 是单次建连的超时（总超时由 client.Timeout 兜底）。
	dialTimeout = 15 * time.Second
	// refusalPrefix 是策略拒绝错误的统一前缀，便于调用方在文案层识别。
	refusalPrefix = "下载被拒绝："
)

// EnvLoopbackKey 是逃生闸的环境变量名：值等于 "1"/"true"（忽略大小写与空白）视为打开。
// 存在它是为了让以子进程方式拉起服务的测试（端到端套件）不必改命令行也能放行 127.0.0.1；
// 生产不设这个环境变量，默认即关闭。
const EnvLoopbackKey = "SUMP_FETCH_ALLOW_LOOPBACK"

// EnvAllowsLoopback 读取逃生闸环境变量。仅供服务启动入口把环境配置并入 Policy 使用。
func EnvAllowsLoopback() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(EnvLoopbackKey)))
	return v == "1" || v == "true"
}

// Refusal 表示「被安全策略拒绝」——区别于网络失败，方便调用层不重复包装成模糊的“下载失败”。
type Refusal struct{ Reason string }

func (r *Refusal) Error() string { return refusalPrefix + r.Reason }

// Policy 是一个服务的下载策略。零值可用（全部默认：生产口径、30s 超时、无兜底上限）。
type Policy struct {
	// AllowLoopback 是仅供测试/显式配置的逃生闸：放行**首个地址**的环回（127.0.0.0/8、::1）。
	// 重定向跳不受它影响（永远严格检查）；私有 / 链路本地 / 组播 / 未指定也照旧拒绝。
	// 默认 false——生产必须保持 false，详见包头注释。
	AllowLoopback bool
	// FallbackMaxBytes 是调用方没给上限（maxBytes<=0）时的兜底上限，
	// 保留各服务原有的口径：images 32MiB、files 100MiB。
	FallbackMaxBytes int64
	// Timeout 是一次下载的总超时；<=0 用默认 30s。
	Timeout time.Duration
}

// Check 校验 rawURL 的协议与地址（按 AllowLoopback 的口径）。通过只代表「值得发起连接」，
// 真正拨号时还会再解析、再检查、并钉住 IP（见 NewClient）——防 DNS rebinding。
func (p Policy) Check(rawURL string) error {
	if r := p.check(rawURL, p.AllowLoopback); r != nil {
		return r
	}
	return nil
}

// check 是共用的校验体。allowLoop 单独成参而不是读 p.AllowLoopback，
// 是为了让「重定向跳」这条路径能强制传 false——逃生闸够不到重定向。
func (p Policy) check(rawURL string, allowLoop bool) *Refusal {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return &Refusal{Reason: fmt.Sprintf("URL 无法解析: %v（url=%q）", err, rawURL)}
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return &Refusal{Reason: fmt.Sprintf("只允许 http/https 协议，收到 %q（url=%q）", u.Scheme, rawURL)}
	}
	return p.checkHost(u, allowLoop)
}

// checkHost 检查 URL 的主机：字面 IP 直接判，域名先解析再逐个判（解析失败也拒绝——地址不明不放行）。
func (p Policy) checkHost(u *url.URL, allowLoop bool) *Refusal {
	host := u.Hostname()
	if host == "" {
		return &Refusal{Reason: fmt.Sprintf("URL 缺少主机名（url=%q）", u.String())}
	}
	if ip := net.ParseIP(host); ip != nil {
		if why := dangerOf(ip, allowLoop); why != "" {
			return &Refusal{Reason: fmt.Sprintf("主机 %s 是%s，禁止访问本机与内网地址（url=%q）", host, why, u.String())}
		}
		return nil
	}
	ips, err := net.DefaultResolver.LookupIP(context.Background(), "ip", host)
	if err != nil || len(ips) == 0 {
		return &Refusal{Reason: fmt.Sprintf("无法解析主机 %q（url=%q）——地址不明时一律拒绝: %v", host, u.String(), err)}
	}
	for _, ip := range ips {
		if why := dangerOf(ip, allowLoop); why != "" {
			return &Refusal{Reason: fmt.Sprintf("主机 %q 解析到 %s（%s），禁止访问本机与内网地址（url=%q）", host, ip, why, u.String())}
		}
	}
	return nil
}

// dangerOf 说明这个 IP 为什么必须被拒绝（返回空串 = 允许访问）。
// 拒绝面钉死在 SSRF 的威胁清单上：环回（本机各服务端口）、私有网段（Go 的 IsPrivate
// 覆盖 RFC1918 与 IPv6 ULA）、链路本地（含云厂商元数据地址 169.254.169.254 / fe80::/10）、
// 未指定（0.0.0.0、::）、组播。
// IPv4 映射的 IPv6（::ffff:127.0.0.1）先归一成 IPv4 再判，换写法混不进来。
func dangerOf(ip net.IP, allowLoop bool) string {
	if ip4 := ip.To4(); ip4 != nil {
		ip = ip4
	}
	switch {
	case ip.IsLoopback():
		if allowLoop {
			return "" // 逃生闸只在这里生效：仅环回、仅首个地址（见 Policy.AllowLoopback）。
		}
		return "环回地址"
	case ip.IsUnspecified():
		return "未指定地址"
	case ip.IsMulticast():
		return "组播地址"
	case ip.IsLinkLocalUnicast(), ip.IsLinkLocalMulticast():
		return "链路本地地址"
	case ip.IsPrivate():
		return "私有网段地址"
	}
	return ""
}

// NewClient 构造一个带守卫的一次性 http.Client：
//   - CheckRedirect：每一跳重定向都按**严格口径**复查（AllowLoopback 不适用于重定向）；
//   - DialContext：建连时重新解析并复查，再拨向检查过的那个 IP（钉住 IP），
//     「校验后、连接前」改解析结果的 DNS rebinding 因此无效；
//   - 不走代理：代理会把「检查过的 IP」变成「由代理决定目标」，钉住就白做了
//     （本仓库的服务都是直连模型，代理不是需求；如需代理请在守卫外层显式设计）。
func (p Policy) NewClient() *http.Client {
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	dialer := &net.Dialer{Timeout: dialTimeout}
	tr := &http.Transport{
		DialContext:       p.dialContext(dialer),
		ForceAttemptHTTP2: true,
		MaxIdleConns:      8,
	}
	return &http.Client{
		Timeout:   timeout,
		Transport: tr,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= maxRedirects {
				return &Refusal{Reason: fmt.Sprintf("重定向超过 %d 跳（起始 url=%q）", maxRedirects, via[0].URL.String())}
			}
			if r := p.check(req.URL.String(), false); r != nil {
				return &Refusal{Reason: fmt.Sprintf("重定向第 %d 跳: %s", len(via), r.Reason)}
			}
			return nil
		},
	}
}

// dialContext 是「复查 + 钉 IP」的建连入口：它永远检查将要真正连接的那个 IP。
func (p Policy) dialContext(dialer *net.Dialer) func(ctx context.Context, network, addr string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("拨号地址无效 %q: %w", addr, err)
		}
		if ip := net.ParseIP(host); ip != nil {
			if why := dangerOf(ip, p.AllowLoopback); why != "" {
				return nil, &Refusal{Reason: fmt.Sprintf("主机 %s 是%s，禁止访问本机与内网地址（目标端口 %s）", host, why, port)}
			}
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		}
		ips, err := net.DefaultResolver.LookupIPAddr(ctx, host)
		if err != nil || len(ips) == 0 {
			return nil, &Refusal{Reason: fmt.Sprintf("无法解析主机 %q（目标端口 %s）——地址不明时一律拒绝: %v", host, port, err)}
		}
		for _, a := range ips {
			if why := dangerOf(a.IP, p.AllowLoopback); why != "" {
				return nil, &Refusal{Reason: fmt.Sprintf("主机 %q 解析到 %s（%s），禁止访问本机与内网地址", host, a.IP, why)}
			}
		}
		// 钉住：只拨这些刚检查过的 IP（逐个试解析结果，类似 Happy Eyeballs），
		// 绝不让底层解析器在「检查之后」重新决定目标。
		var lastErr error
		for _, a := range ips {
			conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(a.IP.String(), port))
			if err == nil {
				return conn, nil
			}
			lastErr = err
			if ctx.Err() != nil {
				break
			}
		}
		return nil, fmt.Errorf("下载失败: 拨向主机 %q 的已检查地址不通: %w", host, lastErr)
	}
}

// Fetch 是 images / files 两个服务共用的下载实现（此前两处近乎复制的 fetchURL 收敛到这里）：
// 限时（默认 30s）、限制大小（多读 1 字节探测超限）、全程带上述守卫。
// maxBytes<=0 时用 Policy.FallbackMaxBytes 兜底（保留各服务原有上限口径）。
// 返回内容体与响应头里的 Content-Type（去掉 "; 参数" 部分）。
func (p Policy) Fetch(rawURL string, maxBytes int64) ([]byte, string, error) {
	if maxBytes <= 0 {
		maxBytes = p.FallbackMaxBytes
	}
	if maxBytes <= 0 {
		return nil, "", &Refusal{Reason: "未配置下载大小上限（maxBytes 与兜底值都无效），拒绝下载"}
	}
	if err := p.Check(rawURL); err != nil {
		return nil, "", err
	}
	resp, err := p.NewClient().Get(rawURL)
	if err != nil {
		// 策略拒绝不套「下载失败」——它不是网络故障，是守卫在起作用，文案要指到具体对象。
		var ref *Refusal
		if errors.As(err, &ref) {
			return nil, "", ref
		}
		return nil, "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		return nil, "", fmt.Errorf("读取下载内容失败: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return nil, "", fmt.Errorf("下载内容超过 %d 字节上限", maxBytes)
	}
	mime := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(mime, ';'); i >= 0 {
		mime = mime[:i]
	}
	return data, strings.TrimSpace(mime), nil
}
