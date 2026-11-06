// 工具并发测试：单会话并发上限（默认 10；-1 = 不限制）；同一服务可并发重复调用。
package tests

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
	"github.com/NouSart/sump/spv2"
)

// stubSlow 是“慢工具”服务替身：每次 work 调用睡一段（默认 80ms），记录并发峰值。
type stubSlow struct {
	srv   *spv2.Server
	url   string
	sleep time.Duration

	mu            sync.Mutex
	inflight      int
	maxConcurrent int
	calls         int
}

func startStubSlow(t *testing.T, center *stubCenter) *stubSlow {
	t.Helper()
	return startStubSlowSleep(t, center, 80*time.Millisecond)
}

// startStubSlowSleep 同 startStubSlow，但可指定单次调用耗时（工具超时行为测试用）。
func startStubSlowSleep(t *testing.T, center *stubCenter, sleep time.Duration) *stubSlow {
	t.Helper()
	e := newStubEndpoint(t, "slow", spv2.Method{Name: "work", Shape: spv2.ShapeCall})
	s := &stubSlow{srv: e.srv, url: e.URL(), sleep: sleep}
	e.handle("work", func(_ context.Context, call *spv2.Call) error {
		s.mu.Lock()
		s.calls++
		s.inflight++
		if s.inflight > s.maxConcurrent {
			s.maxConcurrent = s.inflight
		}
		s.mu.Unlock()

		time.Sleep(s.sleep)

		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()

		return call.Reply(map[string]any{"ok": true})
	})

	registerStub(t, center, protocol.RegisterPayload{
		Name: "slow", Addr: e.URL(), Description: "慢工具测试替身",
		Provides: []protocol.Provide{{Action: "work", Tool: true, Output: "ok"}},
	})
	return s
}

func (s *stubSlow) URL() string { return s.url }

// Stats 返回调用次数与并发峰值。
func (s *stubSlow) Stats() (calls, maxConcurrent int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.maxConcurrent
}

// runSixTools 让一次任务触发 6 个 slow__work 工具调用，返回慢工具替身。
func runSixTools(t *testing.T, limit int) *stubSlow {
	t.Helper()
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	slow := startStubSlow(t, sc)

	var calls []llm.ToolCall
	for i := 0; i < 6; i++ {
		calls = append(calls, llm.ToolCall{
			ID: fmt.Sprintf("call_%d", i), Type: "function",
			Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
		})
	}
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: calls},
		{Role: "assistant", Content: "全部完成"},
	}}

	logger := log.New(os.Stdout, "[conc-test] ", log.LstdFlags)
	l := server.New(server.Config{
		Name: "reasoner", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		ToolConcurrency:   limit,
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := l.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.Shutdown)
	mem.setReasonerURL(l.WsURL())

	sendFrom(t, l, "stub-boss", map[string]any{"text": "跑六个任务"})

	select {
	case d := <-boss.delivered:
		if d.Text != "全部完成" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}
	return slow
}

// TestToolConcurrencyCap 上限 3 时并发峰值应在 2~3；-1（不限制）时峰值应 ≥4。
func TestToolConcurrencyCap(t *testing.T) {
	slow := runSixTools(t, 3)
	calls, max := slow.Stats()
	if calls != 6 {
		t.Fatalf("工具调用次数 = %d, want 6", calls)
	}
	if max < 2 || max > 3 {
		t.Fatalf("上限 3 时并发峰值 = %d, want 2..3", max)
	}

	slow2 := runSixTools(t, -1)
	calls2, max2 := slow2.Stats()
	if calls2 != 6 {
		t.Fatalf("工具调用次数 = %d, want 6", calls2)
	}
	if max2 < 4 {
		t.Fatalf("不限制时并发峰值 = %d, want ≥4", max2)
	}
}
