package tests

// spec §12.3：claim 失败不能只写日志；指标必须有测试真的读到。

import (
	"errors"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

func TestA2AClaimFailureMetric(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "收到"}}}
	s, _, mem := startAttrEnv(t, fake)
	mem.setTask(&protocol.Task{ID: "t-claim", Name: "claim"})
	mem.SetA2AClaimError(errors.New("队列坏了"))
	sendFrom(t, s, "stub-boss", protocol.MessagePayload{
		Text: "继续", Conversation: "conv-claim-metric",
		Sender: &protocol.Sender{Channel: "private", Owner: true},
	})
	found := false
	for _, m := range s.Metrics().Snapshot() {
		if m.Name == "sump_a2a_claim_failures_total" {
			found = true
		}
	}
	if !found {
		t.Fatalf("claim 失败没有进指标: %+v", s.Metrics().Snapshot())
	}
}
