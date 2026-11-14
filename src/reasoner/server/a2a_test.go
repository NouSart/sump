package server

// 同包证据：A2A 的 post / claim / ack 接线（用测试注入点，不依赖网络）。

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestA2APostClaimAckHooks(t *testing.T) {
	s := New(Config{Name: "reasoner"}, log.New(io.Discard, "", 0))
	postedWork, postedText := "", ""
	acked := ""
	claimed := 0
	s.a2a = &a2aHooks{
		post: func(_ context.Context, work, text, trace string) error {
			postedWork, postedText = work, text
			return nil
		},
		claim: func(_ context.Context, work string) (protocol.A2AClaimResult, error) {
			claimed += 1
			if claimed == 1 {
				return protocol.A2AClaimResult{Message: protocol.A2AMessage{
					ID: "m1", WorkID: work, Body: "hi",
					From: protocol.ActorRef{Kind: "agent", ID: "A"},
				}}, nil
			}
			return protocol.A2AClaimResult{}, spv2.ErrCode(spv2.CodeNotFound, "队列空", false, nil)
		},
		ack: func(_ context.Context, id string) error {
			acked = id
			return nil
		},
	}
	ctx := context.Background()
	if err := s.postA2AMessage(ctx, "w1", "hi", "t1"); err != nil {
		t.Fatal(err)
	}
	if postedWork != "w1" || postedText != "hi" {
		t.Fatalf("post 参数不符: %q %q", postedWork, postedText)
	}
	msgs, err := s.claimA2AMessages(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 || !strings.Contains(msgs[0].Content, "（A2A · agent:A）hi") {
		t.Fatalf("claim 结果不符: %+v", msgs)
	}
	if acked != "m1" {
		t.Fatalf("ack 未调用或 id 不符: %q", acked)
	}
}

// message 的 agent 路径：带目标 work 时必须入队并回 queued，不能假装已注入。
func TestMessageAgentWithTargetWorkQueues(t *testing.T) {
	s := New(Config{Name: "reasoner"}, log.New(io.Discard, "", 0))
	posted := ""
	s.a2a = &a2aHooks{post: func(_ context.Context, work, text, trace string) error {
		posted = work + "|" + text
		return nil
	}}
	raw, err := protocol.EncodePayload(protocol.MessagePayload{Text: "进展？", Work: "w-target"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.message(context.Background(), spv2.AgentRef("A"), "w-src", raw)
	if err != nil {
		t.Fatalf("agent message 入队失败: %v", err)
	}
	got, _ := out.(map[string]any)
	if got["queued"] != true {
		t.Fatalf("回执应标 queued: %+v", out)
	}
	if posted != "w-target|进展？" {
		t.Fatalf("post 参数不符: %q", posted)
	}
}

// startTask 必须先把队列消息 claim/注入，再开轮。
func TestStartTaskInjectsA2AQueue(t *testing.T) {
	s := New(Config{Name: "reasoner"}, log.New(io.Discard, "", 0))
	s.a2a = &a2aHooks{
		claim: func(_ context.Context, work string) (protocol.A2AClaimResult, error) {
			if work == "w-a2a" {
				return protocol.A2AClaimResult{Message: protocol.A2AMessage{ID: "m1", Body: "queued-text", From: protocol.ActorRef{Kind: "agent", ID: "A"}}}, nil
			}
			return protocol.A2AClaimResult{}, spv2.ErrCode(spv2.CodeNotFound, "empty", false, nil)
		},
		ack: func(context.Context, string) error { return nil },
	}
	tc := protocol.TaskContext{ConversationID: "c-a2a", Messages: []protocol.Message{{Role: "user", Content: "继续"}}}
	s.startTask(tc, "boss", "w-a2a")
	st := s.boardGet("c-a2a")
	if st == nil {
		t.Fatal("startTask 没开黑板")
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	found := false
	for _, m := range st.messages {
		if strings.Contains(m.Content, "queued-text") {
			found = true
		}
	}
	if !found {
		t.Fatalf("A2A 队列消息没有注入黑板: %+v", st.messages)
	}
}
