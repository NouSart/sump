package tests

// spec §6.8 A2A 最小闭环：agent 消息带目标 work → memory 队列入队；
// 该 work 下一次开任务时 claim→注入→ack（跨 work 消息不丢、不假成功）。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func TestA2AMessageQueuedThenInjected(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "收到"}}}
	s, _, mem := startAttrEnv(t, fake)
	mem.setTask(&protocol.Task{ID: "task-a2a", Name: "A2A"})
	c := dialNative(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 1) agent 带目标 work 发消息：入队，不假装已注入。
	msgCall, err := spv2.NewCall("w-src", spv2.AgentRef("peer-agent"), spv2.ServiceRef("reasoner"), "message", protocol.MessagePayload{
		Text: "进展？", Work: "w-target",
	})
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Status string `msgpack:"status"`
		Queued bool   `msgpack:"queued"`
	}
	if err := c.CallDecode(ctx, msgCall, &ack); err != nil {
		t.Fatalf("A2A message 未被受理: %v", err)
	}
	if ack.Status != "accepted" || !ack.Queued {
		t.Fatalf("A2A message 回执不符: %+v", ack)
	}
	if got := mem.A2AQueueLen("w-target"); got != 1 {
		t.Fatalf("入队数量 = %d, want 1", got)
	}

	// 2) 同一 work 开任务：先 claim/注入/ack，再跑 LLM。
	userCall, err := spv2.NewCall("w-target", spv2.ServiceRef("stub-boss"), spv2.ServiceRef("reasoner"), "message", protocol.MessagePayload{
		Text: "继续", Conversation: "conv-a2a", Sender: &protocol.Sender{Channel: "private", Name: "boss", Owner: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	var rec struct {
		Status string `msgpack:"status"`
	}
	if err := c.CallDecode(ctx, userCall, &rec); err != nil {
		t.Fatalf("message 未被受理: %v", err)
	}
	if rec.Status != "accepted" {
		t.Fatalf("message 回执不符: %+v", rec)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		last := fake.LastRequest()
		if last != nil {
			found := false
			for _, m := range last.Messages {
				if strings.Contains(m.Content, "进展？") && strings.Contains(m.Content, "A2A") {
					found = true
				}
			}
			if found {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	last := fake.LastRequest()
	joined := ""
	if last != nil {
		for _, m := range last.Messages {
			joined += m.Content + "\n"
		}
	}
	if !strings.Contains(joined, "进展？") || !strings.Contains(joined, "A2A") {
		t.Fatalf("队列消息没有注入模型上下文: %q", joined)
	}
	if got := mem.A2AQueueLen("w-target"); got != 0 {
		t.Fatalf("claim 后队列还有 %d 条", got)
	}
	if got := mem.A2AClaimedCount(); got != 0 {
		t.Fatalf("ack 后仍有 %d 条未确认", got)
	}
}
