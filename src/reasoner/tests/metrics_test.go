// Package tests：reasoner 的 LLM / 工具域最小指标（spec §12.3）必须真的出现在 /metrics。
package tests

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

func TestLLMAndToolMetricsOnMetricsEndpoint(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	fake := &llm.Fake{
		Usage: llm.Usage{PromptTokens: 11, CompletionTokens: 7},
		Replies: []llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "c1", Type: "function", Function: llm.FunctionCall{Name: "stub-center__ping", Arguments: "{}"}}}},
			{Role: "assistant", Content: "已经 ping 过了"},
		},
	}
	s := server.New(server.Config{
		Name: "reasoner", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond, Memory: "memory",
		LLM: fake, LLMModel: "frozen-model",
	}, log.New(os.Stdout, "[metrics-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setTask(&protocol.Task{ID: "task-metrics", ConversationID: "default", Name: "ping", Status: "active"})
	mem.setReasonerURL(s.WsURL())

	sendFrom(t, s, "stub-boss", map[string]any{"text": "ping 一下看看"})
	deadline := time.Now().Add(10 * time.Second)
	for fake.CallCount() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if fake.CallCount() < 2 {
		t.Fatalf("LLM 调用次数 = %d, want >=2（工具结果注入后的下一轮）", fake.CallCount())
	}
	_ = boss

	base := "http://" + strings.TrimSuffix(strings.TrimPrefix(s.WsURL(), "ws://"), "/spv2")
	resp, err := http.Get(base + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		"sump_llm_requests_total",
		"sump_llm_latency_seconds",
		"sump_llm_tokens_total",
		`sump_llm_tokens_total{kind="prompt",model="frozen-model"}`,
		`sump_llm_tokens_total{kind="completion",model="frozen-model"}`,
		`sump_llm_requests_total{model="frozen-model",outcome="ok"}`,
		`sump_tool_calls_total{outcome="ok",tool="stub-center__ping"}`,
		`sump_tool_duration_seconds`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("/metrics 缺少 %q\n%s", want, text)
		}
	}
}
