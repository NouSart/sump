package spv2

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPObservability(t *testing.T) {
	srv, err := NewServer(testCard())
	if err != nil {
		t.Fatal(err)
	}
	srv.Metrics().Counter("sump_requests_total", "Total requests", map[string]string{"method": "x", "code": "ok"}, 3)
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("healthz = %d", resp.StatusCode)
	}
	resp.Body.Close()

	resp, err = http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "sump_requests_total") {
		t.Fatalf("metrics = %s", string(body))
	}
}

func TestHTTPReadyzWithoutChecksStaysOK(t *testing.T) {
	srv, err := NewServer(testCard())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	got := readHealthResponse(t, resp)
	if resp.StatusCode != http.StatusOK || got.Status != StatusOK {
		t.Fatalf("readyz = %d status=%q, want 200 ok", resp.StatusCode, got.Status)
	}
	if got.Checks != nil {
		t.Fatalf("checks = %+v, want nil", got.Checks)
	}
}

func TestHTTPHealthzIgnoresHealthChecks(t *testing.T) {
	srv, err := NewServer(testCard(), WithHealthChecks(
		func(context.Context) HealthCheck { return HealthCheck{Name: "db", Status: HealthFail} },
	))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	got := readHealthResponse(t, resp)
	if resp.StatusCode != http.StatusOK || got.Status != StatusOK {
		t.Fatalf("healthz = %d status=%q, want 200 ok", resp.StatusCode, got.Status)
	}
	if got.Checks != nil {
		t.Fatalf("checks = %+v, want nil", got.Checks)
	}
}

func TestHTTPReadyzUsesHealthChecks(t *testing.T) {
	srv, err := NewServer(testCard(), WithHealthChecks(
		func(context.Context) HealthCheck { return HealthCheck{Name: "db", Status: StatusOK} },
		func(context.Context) HealthCheck { return HealthCheck{Name: "center", Status: HealthDegraded} },
	))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	got := readHealthResponse(t, resp)
	if resp.StatusCode != http.StatusOK || got.Status != HealthDegraded {
		t.Fatalf("readyz = %d status=%q, want 200 degraded", resp.StatusCode, got.Status)
	}
	if len(got.Checks) != 2 {
		t.Fatalf("checks = %+v, want 2", got.Checks)
	}
	if got.Checks[0].Name != "db" || got.Checks[0].Status != StatusOK {
		t.Fatalf("db check = %+v", got.Checks[0])
	}
	if got.Checks[1].Name != "center" || got.Checks[1].Status != HealthDegraded {
		t.Fatalf("center check = %+v", got.Checks[1])
	}
	for _, check := range got.Checks {
		if check.LatencyMS < 0 {
			t.Fatalf("latency = %d, want >= 0", check.LatencyMS)
		}
	}
}

func TestHTTPReadyzFailIsUnavailable(t *testing.T) {
	srv, err := NewServer(testCard(), WithHealthChecks(
		func(context.Context) HealthCheck { return HealthCheck{Name: "db", Status: HealthFail} },
	))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	got := readHealthResponse(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || got.Status != HealthFail {
		t.Fatalf("readyz = %d status=%q, want 503 fail", resp.StatusCode, got.Status)
	}
	if len(got.Checks) != 1 || got.Checks[0].Name != "db" || got.Checks[0].Status != HealthFail {
		t.Fatalf("checks = %+v", got.Checks)
	}
}

func TestHealthCheckerPanicIsFail(t *testing.T) {
	srv, err := NewServer(testCard(),
		WithLogger(log.New(io.Discard, "", 0)),
		WithHealthChecks(func(context.Context) HealthCheck { panic("boom") }),
	)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	got := readHealthResponse(t, resp)
	if resp.StatusCode != http.StatusServiceUnavailable || got.Status != HealthFail {
		t.Fatalf("readyz = %d status=%q, want 503 fail", resp.StatusCode, got.Status)
	}
	if len(got.Checks) != 1 || got.Checks[0].Name != "check_1" || got.Checks[0].Status != HealthFail {
		t.Fatalf("checks = %+v", got.Checks)
	}
}

func TestHealthMethodUsesHealthChecks(t *testing.T) {
	_, url := wsStartServer(t, wsTestCard(), WithHealthChecks(
		func(context.Context) HealthCheck { return HealthCheck{Name: "db", Status: HealthDegraded} },
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
	if got.Status != HealthDegraded {
		t.Fatalf("health status = %q, want degraded", got.Status)
	}
	if len(got.Checks) != 1 || got.Checks[0].Name != "db" || got.Checks[0].Status != HealthDegraded {
		t.Fatalf("checks = %+v", got.Checks)
	}
}

func TestNilHealthCheckRejected(t *testing.T) {
	_, err := NewServer(testCard(), WithHealthChecks(nil))
	if err == nil || !strings.Contains(err.Error(), "health 探针") {
		t.Fatalf("err = %v, want health 探针错误", err)
	}
}

func readHealthResponse(t *testing.T, resp *http.Response) HealthResponse {
	t.Helper()
	defer resp.Body.Close()
	var got HealthResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode health response: %v", err)
	}
	return got
}
