package spv2

import (
	"io"
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
