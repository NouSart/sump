package spv2

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Mux 返回一个完整的 HTTP mux：/spv2、/healthz、/readyz、/metrics。
func (s *Server) Mux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("/spv2", s.Handler())
	mux.HandleFunc("/healthz", s.handleHealthHTTP)
	mux.HandleFunc("/readyz", s.handleReadyHTTP)
	mux.HandleFunc("/metrics", s.handleMetricsHTTP)
	return mux
}

func (s *Server) handleHealthHTTP(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Status: "ok", Version: s.card.Version, UptimeMS: time.Since(s.started).Milliseconds(),
	})
}

func (s *Server) handleReadyHTTP(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, HealthResponse{
		Status: "ok", Version: s.card.Version, UptimeMS: time.Since(s.started).Milliseconds(),
	})
}

func (s *Server) handleMetricsHTTP(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(renderOpenMetrics(s.metrics.Snapshot())))
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func renderOpenMetrics(metrics []Metric) string {
	var b strings.Builder
	seen := map[string]bool{}
	for _, m := range metrics {
		if !seen[m.Name] {
			seen[m.Name] = true
			if m.Help != "" {
				fmt.Fprintf(&b, "# HELP %s %s\n", m.Name, m.Help)
			}
			fmt.Fprintf(&b, "# TYPE %s %s\n", m.Name, m.Type)
		}
		switch m.Type {
		case "histogram":
			for _, bucket := range m.Buckets {
				fmt.Fprintf(&b, "%s_bucket%s %d\n", m.Name, labelsWith(m.Labels, "le", strconv.FormatFloat(bucket.LE, 'g', -1, 64)), bucket.Count)
			}
			fmt.Fprintf(&b, "%s_count%s %d\n", m.Name, labels(m.Labels), m.Count)
			fmt.Fprintf(&b, "%s_sum%s %s\n", m.Name, labels(m.Labels), strconv.FormatFloat(m.Sum, 'g', -1, 64))
		default:
			fmt.Fprintf(&b, "%s%s %s\n", m.Name, labels(m.Labels), strconv.FormatFloat(m.Value, 'g', -1, 64))
		}
	}
	return b.String()
}

func labels(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=\"%s\"", k, escapeLabel(m[k])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func labelsWith(m map[string]string, k, v string) string {
	cp := make(map[string]string, len(m)+1)
	for key, val := range m {
		cp[key] = val
	}
	cp[k] = v
	return labels(cp)
}

func escapeLabel(v string) string {
	return strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n").Replace(v)
}
