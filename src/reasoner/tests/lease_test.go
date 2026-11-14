package tests

// 生产接线（spec §6.4）：每次真正执行远端工具前必须先向 memory 扣一次 tool_calls 租约；
// 扣不到租约就不执行工具——不做"先跑再说"的静默降级。

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

func startLeaseEnv(t *testing.T, fake llm.Client) (*server.Server, *stubBoss, *stubMemory, *stubSlow) {
	t.Helper()
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	mem.setTask(&protocol.Task{ID: "task-lease", Name: "租约", Status: "active"})
	slow := startStubSlow(t, sc)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, log.New(os.Stdout, "[lease-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, boss, mem, slow
}

func toolCallSlow() llm.Message {
	return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
		ID: "call_tool", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
	}}}
}

func TestToolCallChargesLeaseBeforeExecution(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		toolCallSlow(),
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_judge", `{"status":"done"}`)}},
		{Role: "assistant", Content: "好的"},
	}}
	s, boss, mem, slow := startLeaseEnv(t, fake)
	sendFrom(t, s, "stub-boss", protocol.MessagePayload{Text: "跑个工具", Conversation: "conv-lease"})

	select {
	case <-boss.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("超时：未交付")
	}
	charges := mem.LeaseCharges()
	if len(charges) != 1 || charges[0].ToolCalls != 1 {
		t.Fatalf("lease_charge 次数/扣减不对: %+v", charges)
	}
	if calls, _ := slow.Stats(); calls != 1 {
		t.Fatalf("工具执行次数 = %d, want 1", calls)
	}
	if got := mem.Works("lease_charge"); len(got) != 1 || len(mem.Works("context")) == 0 || got[0] != mem.Works("context")[0] {
		t.Fatalf("lease_charge 的 work 应与 context 同一 trace: %v / %v", got, mem.Works("context"))
	}
	// boardClose 的 work_update 是异步 Fire：等它到达（经真 WS + 替身）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !containsStr(mem.WorkStatuses(), "done") {
		time.Sleep(20 * time.Millisecond)
	}
	if statuses := mem.WorkStatuses(); !containsStr(statuses, "done") {
		t.Fatalf("work 终态没推进到 done: %v", statuses)
	}
}

func TestLeaseFailureSkipsToolExecution(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		toolCallSlow(),
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_judge", `{"status":"done"}`)}},
		{Role: "assistant", Content: "没跑工具"},
	}}
	s, boss, mem, slow := startLeaseEnv(t, fake)
	mem.setLeaseError("预算已耗尽（测试注入）")
	sendFrom(t, s, "stub-boss", protocol.MessagePayload{Text: "跑个工具", Conversation: "conv-lease-fail"})

	select {
	case <-boss.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("超时：未交付")
	}
	if calls, _ := slow.Stats(); calls != 0 {
		t.Fatalf("扣不到租约却执行了工具: calls=%d", calls)
	}
	if last := fake.LastRequest(); last == nil || !requestHas(*last, "租约") {
		t.Fatalf("模型上下文应看到租约失败原文: %+v", last)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
