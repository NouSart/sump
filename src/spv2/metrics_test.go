package spv2

import "testing"

func TestMetricsSnapshot(t *testing.T) {
	r := NewRegistry()
	r.Counter("sump_requests_total", "req", map[string]string{"method": "m"}, 2)
	r.Gauge("sump_inflight_requests", "inflight", map[string]string{"method": "m"}, 1)
	r.Observe("sump_request_duration_seconds", "lat", "s", map[string]string{"method": "m"}, 0.02)
	snap := r.Snapshot()
	if len(snap) != 3 {
		t.Fatalf("snapshot = %+v", snap)
	}
	for _, m := range snap {
		if m.Name == "sump_request_duration_seconds" && (m.Count != 1 || len(m.Buckets) == 0) {
			t.Fatalf("histogram = %+v", m)
		}
	}
}
