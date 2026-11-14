package spv2

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// spec §12.1/§12.2：/metrics 必须是 OpenMetrics（含 +Inf 桶与 EOF），
// label 只允许低基数白名单，对端可控的 method 不得直接把基数带进指标。

func TestMetricsOpenMetricsFormat(t *testing.T) {
	srv, err := NewServer(testCard())
	if err != nil {
		t.Fatal(err)
	}
	reg := srv.Metrics()
	reg.Counter("sump_test_total", "test counter", map[string]string{"method": "echo"}, 2)
	reg.Observe("sump_test_hist_seconds", "test histogram", "s", map[string]string{"method": "echo"}, 0.001)
	reg.Observe("sump_test_hist_seconds", "test histogram", "s", map[string]string{"method": "echo"}, 100) // 超最后一个有限桶
	ts := httptest.NewServer(srv.Mux())
	defer ts.Close()
	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/openmetrics-text") {
		t.Fatalf("Content-Type = %q, want application/openmetrics-text", ct)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	if !strings.Contains(text, "# EOF") {
		t.Fatalf("OpenMetrics 缺 EOF 标记:\n%s", text)
	}
	if !strings.Contains(text, `le="+Inf"`) || !strings.Contains(text, `sump_test_hist_seconds_bucket`) {
		t.Fatalf("直方图缺 +Inf 桶或计数不对:\n%s", text)
	}
	if !strings.Contains(text, `sump_test_hist_seconds_count{method="echo"} 2`) {
		t.Fatalf("直方图 _count 不对:\n%s", text)
	}
}

func TestMetricsLabelWhitelist(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("sump_test_total", "test", map[string]string{
		"method": "echo", "work": "w1", "actor": "secret", "tenant": "t1", "reason": "ok", "decision": "allow",
	}, 1)
	snap := reg.Snapshot()
	if len(snap) != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}
	labels := snap[0].Labels
	for _, forbidden := range []string{"work", "actor", "tenant"} {
		if _, ok := labels[forbidden]; ok {
			t.Fatalf("禁止的 label %q 进了指标: %+v", forbidden, labels)
		}
	}
	if labels["method"] != "echo" || labels["reason"] != "ok" || labels["decision"] != "allow" {
		t.Fatalf("白名单 label 丢了（§12.3 approval decision 也在内）: %+v", labels)
	}
}

func TestServerUnknownMethodLabelIsBounded(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	for _, method := range []string{"attacker-1", "attacker-2", "attacker-3"} {
		ev, err := NewEvent(wsTestWork, AgentRef("client"), ServiceRef(wsTestService), method, nil)
		if err != nil {
			t.Fatal(err)
		}
		wsWriteFrame(t, conn, ev)
	}
	wsWaitMetric(t, srv, "sump_unknown_event_total", func(m Metric) bool {
		return m.Labels["method"] == "other" && m.Value >= 3
	})
	for _, m := range srv.Metrics().Snapshot() {
		if m.Name != "sump_unknown_event_total" {
			continue
		}
		if got := m.Labels["method"]; got != "other" {
			t.Fatalf("未知 method 的 label = %q, want other（低基数）", got)
		}
	}
	_ = context.Background()
}

// §12.3 的具体指标点名要求 state / kind label（§12.2 通用白名单没列，属于标准内部
// 矛盾；按更具体的指标表执行）。这条断言必须长在 metrics 所在包，才能守住白名单。
func TestMetricsSpecLabelsAllowed(t *testing.T) {
	reg := NewRegistry()
	reg.Counter("sump_a2a_messages_total", "A2A messages", map[string]string{"state": "pending"}, 1)
	reg.Counter("sump_llm_tokens_total", "LLM tokens", map[string]string{"model": "m", "kind": "prompt"}, 2)
	reg.Counter("sump_tool_calls_total", "Tool calls", map[string]string{"tool": "shell__run", "outcome": "ok"}, 1)
	snap := reg.Snapshot()
	seen := map[string]map[string]string{}
	for _, m := range snap {
		seen[m.Name] = m.Labels
	}
	if seen["sump_a2a_messages_total"]["state"] != "pending" {
		t.Fatalf("state label 被白名单丢掉: %+v", seen["sump_a2a_messages_total"])
	}
	if seen["sump_llm_tokens_total"]["kind"] != "prompt" || seen["sump_llm_tokens_total"]["model"] != "m" {
		t.Fatalf("kind/model label 被白名单丢掉: %+v", seen["sump_llm_tokens_total"])
	}
	if seen["sump_tool_calls_total"]["tool"] != "shell__run" {
		t.Fatalf("tool label 被白名单丢掉: %+v", seen["sump_tool_calls_total"])
	}
}
