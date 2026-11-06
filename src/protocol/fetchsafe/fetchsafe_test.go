// fetchsafe 的守卫测试：协议白名单、危险地址、两种逃生闸状态、重定向每一跳、大小上限。
//
// 全部对端都是 httptest（127.0.0.1）与字面 IP——不依赖外网：
// 需要「公网可达」的用例用字面公网 IP 走**校验层**（不真拨号），
// 需要「真下载」的用例配合 AllowLoopback 逃生闸（这正是它的用途，测试态）。
package fetchsafe

import (
	"bytes"
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestCheckRejectsNonHTTPScheme 钉住第一层：非 http/https 协议（file / gopher / ftp / http+unix / 缺协议）
// 必须在文案里点明「协议」，而不是含糊的失败。
func TestCheckRejectsNonHTTPScheme(t *testing.T) {
	p := Policy{FallbackMaxBytes: 1 << 20}
	cases := []struct {
		url  string
		want string // 拒绝文案里必须出现的关键词
	}{
		{"file:///etc/passwd", "协议"},
		{"file:///C:/Windows/win.ini", "协议"},
		{"gopher://127.0.0.1:11211/x", "协议"},
		{"ftp://example.invalid/pub", "协议"},
		{"http+unix:///var/run/docker.sock", "协议"},
		{"//127.0.0.1:9000/spv2", "协议"},
		// 裸 host: 形态连协议都没有——同样必须被拒（此处只要求「被拒绝」，不锁定具体措辞）。
		{"127.0.0.1:9000/spv2", "被拒绝"},
	}
	for _, tc := range cases {
		err := p.Check(tc.url)
		if err == nil {
			t.Fatalf("非 http/https 的 %q 必须被拒绝", tc.url)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q 的拒绝文案应含 %q，实际: %v", tc.url, tc.want, err)
		}
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("%q 的拒绝应是 *Refusal: %T", tc.url, err)
		}
	}
}

// TestCheckRejectsDangerousAddresses 钉住第二层：环回 / 私有 / 链路本地（含元数据地址）/
// 未指定 / 组播，以及换写法混关的形态（IPv6 映射环回、解析到环回的 localhost、
// 数字/十六进制主机名）。默认口径（AllowLoopback=false）全部拒绝。
func TestCheckRejectsDangerousAddresses(t *testing.T) {
	p := Policy{FallbackMaxBytes: 1 << 20}
	cases := []struct {
		url  string
		want string // 拒绝文案里必须出现的关键词
	}{
		{"http://127.0.0.1:9000/spv2", "环回"},
		{"http://127.5.4.3/", "环回"},
		{"http://[::1]/", "环回"},
		{"http://[::ffff:127.0.0.1]:8080/", "环回"}, // IPv4 映射 IPv6 换写法
		{"http://localhost:9000/", "环回"},          // DNS-to-localhost：解析后按 IP 判
		{"http://0.0.0.0/", "未指定"},
		{"http://[::]/", "未指定"},
		{"http://169.254.169.254/latest/meta-data/", "链路本地"},
		{"http://[fe80::1]/", "链路本地"},
		{"http://10.1.2.3/", "私有"},
		{"http://172.16.5.4/", "私有"},
		{"http://192.168.1.1/", "私有"},
		{"http://[fd00::1234]/", "私有"},
		{"http://224.0.0.1/", "组播"},
		{"http://[ff02::1]/", "组播"},
		// 十进制/十六进制环回写法：解析不成名字 → 拒绝；即便某解析器把它认成 127.0.0.1 → 也拒绝。
		{"http://2130706433/", "下载被拒绝"},
		{"http://0x7f000001/", "下载被拒绝"},
	}
	for _, tc := range cases {
		err := p.Check(tc.url)
		if err == nil {
			t.Fatalf("%q 必须被拒绝（默认口径不放行任何本机/内网地址）", tc.url)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%q 的拒绝文案应含 %q，实际: %v", tc.url, tc.want, err)
		}
	}
}

// TestCheckAllowsPublicLiterals 证明守卫不是「一律拒绝」：公网字面 IP 放行（只校验，不拨号）。
func TestCheckAllowsPublicLiterals(t *testing.T) {
	p := Policy{}
	for _, u := range []string{"http://93.184.216.34/a.png", "https://8.8.8.8/x"} {
		if err := p.Check(u); err != nil {
			t.Fatalf("公网地址 %q 不该被拒: %v", u, err)
		}
	}
}

// TestLoopbackHatchStates 覆盖逃生闸的两种状态（同一地址、两套结论）：
// 默认口径拒绝 127.0.0.1；开闸后放行 127.0.0.1（且**只**放行环回——元数据地址照旧拒绝）。
func TestLoopbackHatchStates(t *testing.T) {
	payload := []byte("loopback-under-hatch")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)

	// 状态一：默认（生产口径）——环回被拒，且下载根本没发生。
	def := Policy{FallbackMaxBytes: 1 << 20}
	if err := def.Check(srv.URL); err == nil || !strings.Contains(err.Error(), "环回") {
		t.Fatalf("默认口径必须拒绝环回地址并点明原因，实际: %v", err)
	}
	data, mime, err := def.Fetch(srv.URL, 0)
	if err == nil {
		t.Fatalf("默认口径的 Fetch 必须失败（拿到的是 %d 字节 %q）", len(data), mime)
	}
	var ref *Refusal
	if !errors.As(err, &ref) {
		t.Fatalf("默认口径失败应是策略拒绝而不是网络失败: %v", err)
	}

	// 状态二：开闸（测试口径）——环回放行，内容/协议头都正常取回（Content-Type 去掉参数）。
	open := Policy{AllowLoopback: true, FallbackMaxBytes: 1 << 20}
	data, mime, err = open.Fetch(srv.URL+"/a.png", 0)
	if err != nil {
		t.Fatalf("开闸后对 httptest 对端的下载应成功: %v", err)
	}
	if !bytes.Equal(data, payload) || mime != "image/png" {
		t.Fatalf("下载内容不符: len=%d mime=%q", len(data), mime)
	}

	// 开闸≠开门：闸只覆盖环回，链路本地元数据地址依旧拒绝。
	if err := open.Check("http://169.254.169.254/latest/meta-data/"); err == nil || !strings.Contains(err.Error(), "链路本地") {
		t.Fatalf("逃生闸不该放行元数据地址，实际: %v", err)
	}
}

// TestRedirectHopsStrictlyChecked 钉住第三层在 CheckRedirect 上的口径：
// 即便开了逃生闸，重定向每一跳仍按生产标准检查——公开（这里是闸内环回）中转页
// 302 回本机服务、跳元数据地址、跳非 http 协议，都必须被拒，且文案指到「重定向」。
func TestRedirectHopsStrictlyChecked(t *testing.T) {
	secret := []byte("must-not-be-read")
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(secret)
	}))
	t.Cleanup(target.Close)

	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("to") {
		case "loopback":
			http.Redirect(w, r, target.URL+"/secret", http.StatusFound)
		case "metadata":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data/creds", http.StatusFound)
		case "file":
			http.Redirect(w, r, "file:///etc/passwd", http.StatusFound)
		default:
			http.Error(w, "unknown to", http.StatusBadRequest)
		}
	}))
	t.Cleanup(redirect.Close)

	p := Policy{AllowLoopback: true, FallbackMaxBytes: 1 << 20}
	cases := []struct {
		to   string
		want []string
	}{
		{"loopback", []string{"重定向第 1 跳", "环回"}},
		{"metadata", []string{"重定向第 1 跳", "链路本地"}},
		{"file", []string{"重定向第 1 跳", "协议"}},
	}
	for _, tc := range cases {
		data, _, err := p.Fetch(redirect.URL+"/to?to="+tc.to, 0)
		if err == nil {
			t.Fatalf("重定向到 %s 必须被拒，却拿回了 %d 字节", tc.to, len(data))
		}
		for _, kw := range tc.want {
			if !strings.Contains(err.Error(), kw) {
				t.Fatalf("重定向到 %s 的拒绝文案应含 %q，实际: %v", tc.to, kw, err)
			}
		}
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Fatalf("重定向拒绝应是策略拒绝: %T %v", err, err)
		}
	}
}

// TestDialRefusesHostileLiteralAddress 直接验拨号那一层的复查，不经 Check。
//
// 为什么必须单独验：From URL 出发的话，第一层 Check 已经把危险地址拦掉了，
// 拨号层的复查在测试里"永远轮不到出手"——异变测试把它的复查关掉时全套测试一声不响。
// 而它守的正是那 15 毫秒：Check 看过的是 A，真连接前 DNS 可以换成 B，
// 重定向每一跳也直接落到这里。这一层没人看着，就等于第三层不存在。
func TestDialRefusesHostileLiteralAddress(t *testing.T) {
	dial := Policy{}.dialContext(&net.Dialer{Timeout: time.Second})
	// 每个地址都要拿到 *Refusal（守卫在起作用），不能是"连不上"这种含糊的网络错误。
	for _, addr := range []string{"127.0.0.1:65001", "169.254.169.254:80", "10.0.0.1:65001", "0.0.0.0:80", "[::1]:65001"} {
		_, err := dial(context.Background(), "tcp", addr)
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Errorf("拨向 %s 必须被守卫拒绝，实际错误: %v", addr, err)
		}
	}

	// 逃生闸只把环回放进拨号层，私有网段照旧拒——开闸不等于这一层失守。
	openDial := Policy{AllowLoopback: true}.dialContext(&net.Dialer{Timeout: time.Second})
	if _, err := openDial(context.Background(), "tcp", "10.0.0.1:65001"); err == nil {
		t.Error("开闸后私有网段仍必须被拒")
	} else {
		var ref *Refusal
		if !errors.As(err, &ref) {
			t.Errorf("私有网段要由守卫拒绝而不是网络错误，实际: %v", err)
		}
	}
	if _, err := openDial(context.Background(), "tcp", "127.0.0.1:65001"); err != nil {
		var ref *Refusal
		if errors.As(err, &ref) {
			t.Errorf("开闸后环回不该由守卫拒绝（应是要么连上要么不通）: %v", err)
		}
	}
}

// TestDialChecksResolvedIP 钉住第三层的另一半：拨号口径与校验口径一致——
// localhost（解析到环回）在默认口径下连不上、开闸后连得上（钉住的仍是检查过的 IP）。
func TestDialChecksResolvedIP(t *testing.T) {
	payload := []byte("via-localhost-name")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(payload)
	}))
	t.Cleanup(srv.Close)
	// 把 httptest 的 127.0.0.1 改写成 localhost（同端口）——让解析器来决定。
	localURL := strings.Replace(srv.URL, "127.0.0.1", "localhost", 1)

	def := Policy{FallbackMaxBytes: 1 << 20}
	if _, _, err := def.Fetch(localURL, 0); err == nil {
		t.Fatal("默认口径下 localhost 解析到环回，必须被拒")
	} else if !strings.Contains(err.Error(), "环回") {
		t.Fatalf("拒绝文案应点明环回，实际: %v", err)
	}

	open := Policy{AllowLoopback: true, FallbackMaxBytes: 1 << 20}
	data, _, err := open.Fetch(localURL, 0)
	if err != nil {
		t.Fatalf("开闸后 localhost 形态应可下载: %v", err)
	}
	if !bytes.Equal(data, payload) {
		t.Fatalf("内容不符: %q", data)
	}
}

// TestFetchStatusAndLimits 保留两个服务原有的下载语义：非 200 报状态码、超限报上限、兜底上限生效。
func TestFetchStatusAndLimits(t *testing.T) {
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(failing.Close)
	p := Policy{AllowLoopback: true, FallbackMaxBytes: 1 << 20}
	if _, _, err := p.Fetch(failing.URL+"/x", 0); err == nil || !strings.Contains(err.Error(), "HTTP 502") {
		t.Fatalf("非 200 应报状态码，实际: %v", err)
	}

	body := bytes.Repeat([]byte("a"), 4096)
	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(big.Close)
	if _, _, err := p.Fetch(big.URL+"/x", 1024); err == nil || !strings.Contains(err.Error(), "超过 1024 字节上限") {
		t.Fatalf("超限应报字节上限，实际: %v", err)
	}
	// maxBytes<=0 时兜底上限生效（这里把兜底设得比正文还小，模拟服务侧的默认口径）。
	fallback := Policy{AllowLoopback: true, FallbackMaxBytes: 64}
	if _, _, err := fallback.Fetch(big.URL+"/x", 0); err == nil || !strings.Contains(err.Error(), "超过 64 字节上限") {
		t.Fatalf("兜底上限应生效，实际: %v", err)
	}
	// 两个上限都无效 → 明确拒绝，绝不静默不限大小。
	noFallback := Policy{AllowLoopback: true}
	if _, _, err := noFallback.Fetch(big.URL+"/x", 0); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("没有任何上限时必须拒绝下载，实际: %v", err)
	}

	// 恰好等于上限：允许（与历史行为一致——多读 1 字节只用来探测「超过」）。
	if data, _, err := p.Fetch(big.URL+"/x", 4096); err != nil || len(data) != 4096 {
		t.Fatalf("恰好等于上限应放行: len=%d err=%v", len(data), err)
	}
}

// TestEnvAllowsLoopback 固定逃生闸环境变量的口径：只认 1/true（忽略大小写与空白），其余一律关闭。
func TestEnvAllowsLoopback(t *testing.T) {
	for v, want := range map[string]bool{
		"":      false,
		"0":     false,
		"false": false,
		"yes":   false,
		"1":     true,
		"true":  true,
		"TRUE":  true,
		" 1 ":   true,
		"true ": true,
	} {
		t.Setenv(EnvLoopbackKey, v)
		if got := EnvAllowsLoopback(); got != want {
			t.Fatalf("SUMP_FETCH_ALLOW_LOOPBACK=%q: got %v want %v", v, got, want)
		}
	}
}
