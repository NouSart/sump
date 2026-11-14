package spv2

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 这一组用例补的是「文档承诺了但原测试测不到」的边界：探针实测、checks 省略、
// 聚合优先级、未知状态 fail-closed、多组注入、healthz 不碰依赖、panic 留痕、
// 探针超时。每一条都对应一次可存活的异变（见 audit-2026-11-14）。

func startHealthMux(t *testing.T, opts ...ServerOption) *httptest.Server {
	t.Helper()
	srv, err := NewServer(testCard(), opts...)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	t.Cleanup(ts.Close)
	return ts
}

func fetchReadyRaw(t *testing.T, url string) (*http.Response, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, body
}

func TestHTTPReadyzAllChecksOK(t *testing.T) {
	ts := startHealthMux(t, WithHealthChecks(
		func(context.Context) HealthCheck { return HealthCheck{Name: "db", Status: StatusOK} },
	))
	resp, body := fetchReadyRaw(t, ts.URL+"/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d body=%s", resp.StatusCode, body)
	}
	var got HealthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK || len(got.Checks) != 1 || got.Checks[0].Status != StatusOK {
		t.Fatalf("readyz body = %+v", got)
	}
	// latency_ms 必须真实出现在线上（0 也要出现，不能因 omitempty 消失）。
	var raw struct {
		Checks []map[string]json.RawMessage `json:"checks"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw.Checks[0]["latency_ms"]; !ok {
		t.Fatalf("checks[0] 缺 latency_ms: %s", body)
	}
}

func TestHTTPReadyzOmitsChecksKey(t *testing.T) {
	ts := startHealthMux(t)
	_, body := fetchReadyRaw(t, ts.URL+"/readyz")
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatal(err)
	}
	if _, ok := raw["checks"]; ok {
		t.Fatalf("没有探针时 checks 必须省略，实际: %s", body)
	}
}

func TestHTTPReadyzFailDominatesDegraded(t *testing.T) {
	probe := func(name, status string) HealthChecker {
		return func(context.Context) HealthCheck { return HealthCheck{Name: name, Status: status} }
	}
	for _, tc := range []struct {
		name  string
		order []HealthChecker
	}{
		{"fail 在前", []HealthChecker{probe("a", HealthFail), probe("b", HealthDegraded)}},
		{"fail 在后", []HealthChecker{probe("a", HealthDegraded), probe("b", HealthFail)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ts := startHealthMux(t, WithHealthChecks(tc.order...))
			resp, body := fetchReadyRaw(t, ts.URL+"/readyz")
			if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), `"status":"fail"`) {
				t.Fatalf("readyz = %d body=%s, want 503 fail", resp.StatusCode, body)
			}
		})
	}
}

func TestHTTPReadyzUnknownCheckStatusIsFail(t *testing.T) {
	ts := startHealthMux(t, WithHealthChecks(func(context.Context) HealthCheck {
		return HealthCheck{Name: "weird", Status: "unhealthy"}
	}))
	resp, body := fetchReadyRaw(t, ts.URL+"/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d body=%s, want 503", resp.StatusCode, body)
	}
	var got HealthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Status != HealthFail || len(got.Checks) != 1 || got.Checks[0].Status != HealthFail {
		t.Fatalf("未知状态没有 fail-closed: %+v", got)
	}
}

func TestWithHealthChecksAccumulates(t *testing.T) {
	ts := startHealthMux(t,
		WithHealthChecks(func(context.Context) HealthCheck { return HealthCheck{Name: "a", Status: StatusOK} }),
		WithHealthChecks(func(context.Context) HealthCheck { return HealthCheck{Name: "b", Status: HealthDegraded} }),
	)
	resp, body := fetchReadyRaw(t, ts.URL+"/readyz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("readyz = %d body=%s", resp.StatusCode, body)
	}
	var got HealthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Checks) != 2 || got.Checks[0].Name != "a" || got.Checks[1].Name != "b" {
		t.Fatalf("两次 WithHealthChecks 没有叠加: %+v", got.Checks)
	}
}

func TestHTTPHealthzDoesNotRunProbes(t *testing.T) {
	var calls atomic.Int32
	ts := startHealthMux(t, WithHealthChecks(func(context.Context) HealthCheck {
		calls.Add(1)
		return HealthCheck{Name: "db", Status: StatusOK}
	}))
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 0 {
		t.Fatalf("healthz 执行了 %d 次探针（纯存活检查不得碰依赖）", got)
	}
	resp, err = http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got := calls.Load(); got != 1 {
		t.Fatalf("readyz 执行探针 %d 次, want 1", got)
	}
}

func TestHealthProbeLatencyIsMeasured(t *testing.T) {
	ts := startHealthMux(t, WithHealthChecks(func(context.Context) HealthCheck {
		time.Sleep(25 * time.Millisecond)
		return HealthCheck{Name: "slow", Status: StatusOK}
	}))
	_, body := fetchReadyRaw(t, ts.URL+"/readyz")
	var got HealthResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatal(err)
	}
	if got.Checks[0].LatencyMS < 10 {
		t.Fatalf("latency_ms = %d, 探针睡了 25ms，实测值必须被填进来", got.Checks[0].LatencyMS)
	}
}

func TestHealthProbePanicIsLogged(t *testing.T) {
	var logged lockedBuffer
	ts := startHealthMux(t,
		WithLogger(log.New(&logged, "", 0)),
		WithHealthChecks(func(context.Context) HealthCheck { panic("boom") }),
	)
	resp, _ := fetchReadyRaw(t, ts.URL+"/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", resp.StatusCode)
	}
	logs := logged.String()
	if !strings.Contains(logs, "boom") {
		t.Fatalf("探针 panic 没有落日志: %q", logs)
	}
}

func TestHealthMethodUsesCallerContext(t *testing.T) {
	ctxCh := make(chan context.Context, 1)
	_, url := wsStartServer(t, wsTestCard(), WithActionTimeout(2*time.Second), WithHealthChecks(
		func(ctx context.Context) HealthCheck {
			ctxCh <- ctx
			return HealthCheck{Name: "db", Status: StatusOK}
		},
	))
	c := wsDialClient(t, url)
	f, err := NewCall(wsTestWork, HumanRef("portal"), ServiceRef(wsTestService), "health", nil)
	if err != nil {
		t.Fatal(err)
	}
	var got HealthResponse
	if err := c.CallDecode(timeoutCtx(t), f, &got); err != nil {
		t.Fatal(err)
	}
	ctx := <-ctxCh
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("health 探针拿到的 ctx 没有 deadline，§9.3 的到点约束失效")
	}
}

func TestReadyzHungProbeBounded(t *testing.T) {
	block := make(chan struct{})
	ts := startHealthMux(t,
		WithHealthTimeout(60*time.Millisecond),
		WithHealthChecks(func(context.Context) HealthCheck { <-block; return HealthCheck{Status: StatusOK} }),
	)
	// 先放阻塞探针再关 httptest（LIFO），否则挂住的 handler 会让 server.Close 等死。
	t.Cleanup(func() { close(block) })
	client := &http.Client{Timeout: 2 * time.Second}
	start := time.Now()
	resp, err := client.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("探针挂住时 /readyz 必须在上限内回 503，实际错误: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("readyz = %d, want 503", resp.StatusCode)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("readyz 用了 %v，健康超时没有生效", elapsed)
	}
}
