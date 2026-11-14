package server

import (
	"testing"
	"time"

	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

// spec §12.3：LLM / 工具域最小指标必须能在注册表上被直接验证（不依赖起服务）。
func TestRecordLLMMetrics(t *testing.T) {
	reg := spv2.NewRegistry()
	recordLLMTo(reg, "m1", 20*time.Millisecond, llm.Usage{PromptTokens: 11, CompletionTokens: 7}, nil)
	recordLLMTo(reg, "m1", 10*time.Millisecond, llm.Usage{}, errFake)
	ms := reg.Snapshot()
	find := func(name string, labels map[string]string) *spv2.Metric {
		for i := range ms {
			if ms[i].Name != name {
				continue
			}
			ok := true
			for k, v := range labels {
				if ms[i].Labels[k] != v {
					ok = false
				}
			}
			if ok {
				return &ms[i]
			}
		}
		return nil
	}
	if m := find("sump_llm_requests_total", map[string]string{"model": "m1", "outcome": "ok"}); m == nil || m.Value != 1 {
		t.Fatalf("ok 请求计数 = %+v", m)
	}
	if m := find("sump_llm_requests_total", map[string]string{"model": "m1", "outcome": "error"}); m == nil || m.Value != 1 {
		t.Fatalf("error 请求计数 = %+v", m)
	}
	if m := find("sump_llm_tokens_total", map[string]string{"model": "m1", "kind": "prompt"}); m == nil || m.Value != 11 {
		t.Fatalf("prompt tokens = %+v", m)
	}
	if m := find("sump_llm_tokens_total", map[string]string{"model": "m1", "kind": "completion"}); m == nil || m.Value != 7 {
		t.Fatalf("completion tokens = %+v", m)
	}
	if m := find("sump_llm_latency_seconds", map[string]string{"model": "m1"}); m == nil || m.Count != 2 {
		t.Fatalf("latency histogram = %+v", m)
	}
}

func TestRecordToolMetrics(t *testing.T) {
	reg := spv2.NewRegistry()
	recordToolTo(reg, "shell__run", 5*time.Millisecond, nil)
	recordToolTo(reg, "shell__run", 30*time.Millisecond, errFake)
	ms := reg.Snapshot()
	seen := map[string]float64{}
	for _, m := range ms {
		if m.Name == "sump_tool_calls_total" {
			seen[m.Labels["outcome"]] = m.Value
		}
	}
	if seen["ok"] != 1 || seen["error"] != 1 {
		t.Fatalf("tool calls = %+v", seen)
	}
	found := false
	for _, m := range ms {
		if m.Name == "sump_tool_duration_seconds" && m.Labels["tool"] == "shell__run" {
			found = true
		}
	}
	if !found {
		t.Fatal("缺 sump_tool_duration_seconds{tool=shell__run}")
	}
}

var errFake = &spv2.Error{Code: spv2.CodeInternal, Message: "fake"}
