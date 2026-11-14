package tests

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// spv2.md §12.1：服务真实监听面必须提供 /healthz /readyz /metrics；
// /readyz 要跑骨架 center 探针与服务注入的探针。
func TestServiceServesHTTPObservabilityEndpoints(t *testing.T) {
	sc := startStubCenter(t)
	s := service.New(service.Config{
		Name:       "obs",
		Listen:     "127.0.0.1:0",
		Center:     sc.URL(),
		SecretFile: "-",
		Provides:   []protocol.Provide{{Action: "echo", Tool: true, Input: "{text}", Output: "{text}"}},
	})
	s.HandleNative(spv2.Method{Name: "echo", Tool: true, Input: map[string]any{"type": "object"}, Output: map[string]any{"type": "object"}},
		func(_ context.Context, call *spv2.Call) error { return call.Reply("ok") })
	s.AddHealthCheck(func(context.Context) spv2.HealthCheck {
		return spv2.HealthCheck{Name: "db", Status: spv2.StatusOK}
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	base := "http://" + strings.TrimSuffix(strings.TrimPrefix(s.WsURL(), "ws://"), "/spv2")
	get := func(path string) (int, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(body)
	}

	if code, body := get("/healthz"); code != http.StatusOK || !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("healthz = %d %s", code, body)
	}
	code, body := get("/readyz")
	if code != http.StatusOK {
		t.Fatalf("readyz = %d %s, want 200（center 已连接）", code, body)
	}
	if !strings.Contains(body, `"name":"center"`) || !strings.Contains(body, `"name":"db"`) {
		t.Fatalf("readyz 没跑骨架/注入探针: %s", body)
	}
	// 产生一跳真实请求 + 一条错误，让传输/错误/连接指标都出现（spec §12.3/§12.4）。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, err := spv2.Dial(ctx, s.WsURL())
	if err != nil {
		t.Fatalf("dial self: %v", err)
	}
	okCall, err := spv2.NewCall("w-health", spv2.ServiceRef("obs-probe"), spv2.ServiceRef("obs"), "health", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, okCall); err != nil {
		t.Fatalf("health call: %v", err)
	}
	badCall, err := spv2.NewCall("w-bad", spv2.ServiceRef("obs-probe"), spv2.ServiceRef("obs"), "no_such_method", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Call(ctx, badCall); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("unknown method err = %v, want not_found", err)
	}
	_ = client.Close()

	code, metricsBody := get("/metrics")
	if code != http.StatusOK {
		t.Fatalf("metrics = %d", code)
	}
	// 骨架必须自动埋的最小指标集（spec §12.3 传输/连接/错误 + §12.4 契约测试）。
	for _, name := range []string{
		"sump_requests_total",
		"sump_request_duration_seconds",
		"sump_inflight_requests",
		"sump_errors_total",
		"sump_request_bytes",
		"sump_response_bytes",
		"sump_connection_state",
		"sump_reconnects_total",
		"sump_heartbeat_age_seconds",
	} {
		if !strings.Contains(metricsBody, name) {
			t.Errorf("最小指标集缺少 %s", name)
		}
	}
}
