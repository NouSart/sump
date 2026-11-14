package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/NouSart/sump/spv2"
)

// spv2.md §12.1/§7.6：生产骨架必须挂 HTTP 观测端点，/readyz 必须真的跑依赖探针。
func TestServiceMuxServesObservabilityWithProbes(t *testing.T) {
	peer := New(Config{Name: "peer", Provides: bridgePeerCard("peer"), SecretFile: "-"})
	peer.AddHealthCheck(func(context.Context) spv2.HealthCheck {
		return spv2.HealthCheck{Name: "db", Status: spv2.StatusOK}
	})
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(peer.spv2Srv.Mux())
	defer ts.Close()

	get := func(path string) (*http.Response, spv2.HealthResponse) {
		t.Helper()
		resp, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var got spv2.HealthResponse
		if path != "/metrics" {
			if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
				t.Fatalf("%s decode: %v", path, err)
			}
		}
		return resp, got
	}

	if resp, got := get("/healthz"); resp.StatusCode != http.StatusOK || got.Status != spv2.StatusOK {
		t.Fatalf("healthz = %d %+v", resp.StatusCode, got)
	}
	// center 未连接：readyz 必须 503；注入的 db 探针也要出现在 checks 里。
	resp, got := get("/readyz")
	if resp.StatusCode != http.StatusServiceUnavailable || got.Status != spv2.HealthFail {
		t.Fatalf("readyz = %d %+v, want 503 fail", resp.StatusCode, got)
	}
	statuses := map[string]string{}
	for _, c := range got.Checks {
		statuses[c.Name] = c.Status
	}
	if statuses["center"] != spv2.HealthFail {
		t.Fatalf("center 探针未反映未连接: %+v", got.Checks)
	}
	if statuses["db"] != spv2.StatusOK {
		t.Fatalf("注入的 db 探针没有执行: %+v", got.Checks)
	}
	if resp, _ := get("/metrics"); resp.StatusCode != http.StatusOK {
		t.Fatalf("metrics = %d", resp.StatusCode)
	}
}
