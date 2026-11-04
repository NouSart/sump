package tests

// 任务门禁：agent 在任务里工作，出结果必须先判定任务是否完成。
// 1) 有任务但没判定 → 拒绝交付，并把"先判定"的提醒注入下一轮；
// 2) 调 task_status(done) 后再交付 → 通过，且把任务完结推给记忆；
// 3) 系统提示里带着任务名（agent 知道自己在为哪个任务工作）。

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

func taskStatusCall(id, args string) llm.ToolCall {
	return llm.ToolCall{ID: id, Type: "function", Function: llm.FunctionCall{Name: "task_status", Arguments: args}}
}

func TestTaskGate(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	mem.setTask(&protocol.Task{ID: "task-1", Name: "看看现在几点", Status: "active"})

	fake := &llm.Fake{Replies: []llm.Message{
		// 第 1 轮：直接交付，没判定任务 → 应被门禁拒绝
		{Role: "assistant", Content: "现在是十点。"},
		// 第 2 轮：补上判定（done，并顺手把标题写准）
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_task", `{"status":"done","title":"查一下现在几点","note":"已经答了"}`)}},
		// 第 3 轮：交付
		{Role: "assistant", Content: "现在是十点整。"},
	}}

	logger := log.New(os.Stdout, "[task-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(map[string]any{"text": "现在几点了"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "stub-boss"
	resp, err := c.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.ResponsePayload
	if err := resp.DecodePayload(&ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Fatalf("user_message 未被受理: %s", ack.Error)
	}

	// 最终交付必须是"补判定之后"那一轮的内容。
	select {
	case d := <-boss.delivered:
		if d.Text != "现在是十点整。" {
			t.Fatalf("交付内容不符（说明门禁没拦住第一轮）: %q", d.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if fake.CallCount() != 3 {
		t.Fatalf("Fake LLM 调用次数 = %d, want 3（交付 → 判定 → 交付）", fake.CallCount())
	}

	// 第 2 轮请求里应带着任务名和"先判定"的提醒。
	second := fake.Requests[1]
	var hasTaskPrompt, hasReminder, hasTool bool
	for _, m := range second.Messages {
		if strings.Contains(m.Content, "当前任务：《看看现在几点》") {
			hasTaskPrompt = true
		}
		if strings.Contains(m.Content, "task_status") && strings.Contains(m.Content, "还没有调用") {
			hasReminder = true
		}
	}
	for _, tool := range second.Tools {
		if tool.Function.Name == "task_status" {
			hasTool = true
		}
	}
	if !hasTaskPrompt {
		t.Fatalf("系统提示应带任务名: %+v", second.Messages)
	}
	if !hasReminder {
		t.Fatalf("第二轮应注入「先判定」提醒: %+v", second.Messages)
	}
	if !hasTool {
		t.Fatalf("工具列表应包含内置 task_status: %+v", second.Tools)
	}

	// 判定 done → 任务完结推给记忆，且带回模型给的新标题。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(mem.Finished()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	fin := mem.Finished()
	if len(fin) != 1 || fin[0].TaskID != "task-1" || fin[0].Name != "查一下现在几点" {
		t.Fatalf("task_finish 不符: %+v", fin)
	}
}

// TestTaskContinue 验证 continue 判定：任务保持进行中（只改名、不完结）。
func TestTaskContinue(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	mem.setTask(&protocol.Task{ID: "task-2", Name: "旧标题", Status: "active"})

	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_t", `{"status":"continue","title":"分两步做的事"}`)}},
		{Role: "assistant", Content: "先做了第一步。"},
	}}

	logger := log.New(os.Stdout, "[task-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name: "reasoner", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond, Memory: "memory", LLM: fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)
	raw, _ := protocol.EncodePayload(map[string]any{"text": "开始做"})
	req, _ := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	req.Boss = "stub-boss"
	if _, err := c.Call(ctx, req); err != nil {
		t.Fatal(err)
	}

	select {
	case d := <-boss.delivered:
		if d.Text != "先做了第一步。" {
			t.Fatalf("交付内容不符: %q", d.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(mem.Renamed()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	ren := mem.Renamed()
	if len(ren) != 1 || ren[0].TaskID != "task-2" || ren[0].Name != "分两步做的事" {
		t.Fatalf("task_rename 不符: %+v", ren)
	}
	if len(mem.Finished()) != 0 {
		t.Fatalf("continue 不该完结任务: %+v", mem.Finished())
	}
}
