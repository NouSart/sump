// reasoner 运行时设置（configure 下发）的**行为级**测试：
// 不只看“回执成功 / 生效值已记录”，而是验证设置真的改变了运行行为——
// tool.concurrency 真正约束并发峰值；tool.timeout_ms 真正约束单次工具执行时长。
package tests

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

// startSlowReasoner 起一套最小环境（中心 / boss / 记忆 / 慢工具替身 + reasoner），
// 返回 reasoner 与慢工具替身，供“先 configure 再跑任务”的行为测试使用。
func startSlowReasoner(t *testing.T, fake *llm.Fake, slowSleep time.Duration, limit int) (*server.Server, *stubSlow, *stubBoss) {
	t.Helper()
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	slow := startStubSlowSleep(t, sc, slowSleep)

	logger := log.New(os.Stdout, "[settings-live-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		ToolConcurrency:   limit,
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, slow, boss
}

// configure 下发一个设置项并断言被受理。
func configure(t *testing.T, s *server.Server, p protocol.ConfigurePayload) {
	t.Helper()
	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rp := jump(t, c, "configure", p, ctx)
	if !rp.OK {
		t.Fatalf("configure 未被受理: %s", rp.Error)
	}
}

// TestConcurrencyConfigureLive：以 -1（不限制）启动，下发 tool.concurrency=2 后，
// 同一会话的并发峰值应恰为 2——设置真正约束了运行行为（不限制时峰值 ≥4，见 TestToolConcurrencyCap）。
func TestConcurrencyConfigureLive(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: sixSlowCalls()},
		{Role: "assistant", Content: "全部完成"},
	}}
	s, slow, boss := startSlowReasoner(t, fake, 80*time.Millisecond, -1)

	configure(t, s, protocol.ConfigurePayload{Key: "tool.concurrency", Value: "2", Default: "10", Overridden: true})

	sendUser(t, s, "跑六个任务")
	select {
	case d := <-boss.delivered:
		if d.Text != "全部完成" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	calls, max := slow.Stats()
	if calls != 6 {
		t.Fatalf("工具调用次数 = %d, want 6", calls)
	}
	if max != 2 {
		t.Fatalf("configure 的并发上限未生效：峰值 = %d, want 2", max)
	}
}

// TestToolTimeoutConfigureLive：下发 tool.timeout_ms=1000 后，
// 耗时 2s 的工具调用按失败入账（[工具结果] 含“错误”），而不是无限等待——
// 设置真正改变了单次工具执行的超时（2s vs 1s：留 1s 余量防调度抖动；测试耗时不变，等待方是超时路径）。
func TestToolTimeoutConfigureLive(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_0", Type: "function",
			Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "收尾"},
	}}
	s, slow, boss := startSlowReasoner(t, fake, 2*time.Second, 0)

	configure(t, s, protocol.ConfigurePayload{Key: "tool.timeout_ms", Value: "1000", Default: "60000", Overridden: true})

	sendUser(t, s, "跑一个超过 1 秒的慢任务")
	select {
	case d := <-boss.delivered:
		if d.Text != "收尾" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if calls, _ := slow.Stats(); calls != 1 {
		t.Fatalf("慢工具应被调用 1 次，实际 %d", calls)
	}
	// 超时结果按失败入账：最后一轮请求里该调用的 [工具结果] 应含“错误”。
	last := *fake.LastRequest()
	found := false
	for _, m := range last.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "[工具结果]") && strings.Contains(m.Content, "call_0") {
			found = true
			if !strings.Contains(m.Content, "错误") {
				t.Fatalf("超时结果应为失败入账: %q", m.Content)
			}
		}
	}
	if !found {
		t.Fatalf("最后一轮请求应含 call_0 的工具结果: %+v", last.Messages)
	}
}

// sixSlowCalls 构造 6 个 slow__work 工具调用。
func sixSlowCalls() []llm.ToolCall {
	calls := make([]llm.ToolCall, 0, 6)
	for i := 0; i < 6; i++ {
		calls = append(calls, llm.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Type: "function",
			Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
		})
	}
	return calls
}
