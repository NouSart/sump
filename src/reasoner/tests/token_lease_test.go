package tests

// spec §6.4：LLM usage 必须计入 token 租约；provider 不回 usage 时不凭空记账。

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

func TestLLMUsageChargesTokenLease(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	mem.setTask(&protocol.Task{ID: "task-tokens", Name: "token", Status: "active"})
	fake := &llm.Fake{
		Usage: llm.Usage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		Replies: []llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_judge", `{"status":"done"}`)}},
			{Role: "assistant", Content: "好的"},
		},
	}
	s := server.New(server.Config{
		Name: "reasoner", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond, Memory: "memory", LLM: fake,
	}, log.New(os.Stdout, "[token-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	sendFrom(t, s, "stub-boss", protocol.MessagePayload{Text: "说句话", Conversation: "conv-token"})
	select {
	case <-boss.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("超时：未交付")
	}
	total := 0
	for _, c := range mem.LeaseCharges() {
		total += c.Tokens
	}
	if total != 30 {
		t.Fatalf("token 租约扣减 = %d, want 30（两轮 LLM 各 15）: %+v", total, mem.LeaseCharges())
	}
}
