package tests

// 能力族是**统计**出来的，不是模型说的。
//
// 这条用例盯的正是那条最容易变成"许愿"的路径：复盘要写 capabilities，
// 如果 reasoner 拿的是一份**不含工具调用**的消息快照，它会安静地写一个空数组——
// 功能看起来"实现了"，目录里却永远没有"用到 shell / files"。
//
// 所以这里真跑一次工具调用（stub 服务 registed 成工具），再断言发布出去的
// capabilities 里真的有那个服务名。

import (
	"context"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

// callingLLM 是"先用一次工具、再判定、再收尾"的脚本模型。
type callingLLM struct {
	mu    sync.Mutex
	turn  int
	tools []string // 第一轮里发起的工具名
}

func (c *callingLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.Message, error) {
	isReflect := false
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "复盘器") {
			isReflect = true
			break
		}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if isReflect {
		return llm.Message{Role: "assistant", Content: `{"digest":{"goal":"跑了一次工具","outcome":"成功"},"notes":[]}`}, nil
	}
	c.turn++
	switch c.turn {
	case 1:
		var calls []llm.ToolCall
		for i, name := range c.tools {
			calls = append(calls, llm.ToolCall{
				ID:       "call_" + name + string(rune('a'+i)),
				Type:     "function",
				Function: llm.FunctionCall{Name: name, Arguments: `{}`},
			})
		}
		return llm.Message{Role: "assistant", ToolCalls: calls}, nil
	case 2:
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_judge", Type: "function",
			Function: llm.FunctionCall{Name: "task_status", Arguments: `{"status":"done"}`},
		}}}, nil
	default:
		return llm.Message{Role: "assistant", Content: "任务答复"}, nil
	}
}

func TestReflectPublishesCapabilitiesFromToolCalls(t *testing.T) {
	sc := startStubCenter(t)
	client := &callingLLM{tools: []string{"slow__work"}}
	// 先把工具登记进名册，再起 reasoner：它注册时拿到的名册里就有 slow__work。
	slow := startStubSlowSleep(t, sc, 5*time.Millisecond)

	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	cfg := server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		NotifyMode:        "each",
		LLM:               client,
		Reflect:           true,
		Share:             true,
	}
	s := server.New(cfg, log.New(os.Stdout, "[cap-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	mem.setTask(&protocol.Task{ID: "task-cap", ConversationID: "default", Name: "跑工具", Status: "active"})

	sendUser(t, s, "跑一下工具")
	waitDelivery(t, boss, "任务答复")

	pubs := publishEventually(t, mem, 1, 10*time.Second)
	if len(pubs) != 1 {
		t.Fatalf("应发布 1 条摘要，实际 %d: %+v", len(pubs), pubs)
	}
	// 先证明这条用例不是空转：工具真的被调用了（否则"空 capabilities"也可能是没跑到）。
	if calls, _ := slow.Stats(); calls == 0 {
		t.Fatal("工具一次都没被调用：这条用例没验证到任何东西")
	}
	caps := pubs[0].Capabilities
	if len(caps) != 1 || caps[0] != "slow" {
		t.Fatalf("capabilities 应从真实工具调用统计出来（期望 [slow]），实际 %+v", caps)
	}
	// 内置工具（task_status）不该混进能力族——它不是网络上的服务。
	for _, c := range caps {
		if c == "task_status" || c == "wait" {
			t.Fatalf("内置工具不该进能力族: %+v", caps)
		}
	}
}
