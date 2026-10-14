// 工具并发测试：单会话并发上限（默认 10；-1 = 不限制）；同一服务可并发重复调用。
package tests

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

// stubSlow 是“慢工具”服务替身：每次 work 调用睡一段（默认 80ms），记录并发峰值。
type stubSlow struct {
	ln  net.Listener
	srv *http.Server

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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &stubSlow{ln: ln, sleep: sleep}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	s.srv = &http.Server{Handler: mux}
	go func() { _ = s.srv.Serve(ln) }()
	t.Cleanup(func() { _ = s.srv.Close() })

	center.mu.Lock()
	center.cards["slow"] = protocol.ServiceCard{
		Name: "slow", Addr: s.URL(), Description: "慢工具测试替身",
		Provides: []protocol.Provide{{Action: "work", Tool: true, Output: "ok"}},
	}
	center.mu.Unlock()
	return s
}

func (s *stubSlow) URL() string { return "ws://" + s.ln.Addr().String() + "/ws" }

func (s *stubSlow) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		if jp.Action != "work" {
			continue
		}

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

		resp, _ := protocol.NewResponse(env, "slow", protocol.ResponsePayload{OK: true})
		c.send(resp)
	}
}

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

	c := dialReasoner(t, l)
	raw, err := protocol.EncodePayload(map[string]any{"text": "跑六个任务"})
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
