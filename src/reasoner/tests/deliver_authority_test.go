package tests

// spec §6.5：deliver 的帧头必须是 work.agent + authority=runtime（entry 按这个校验来源）。

import (
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func TestDeliverUsesAgentRuntimeAuthority(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("judge", `{"status":"done"}`)}}, {Role: "assistant", Content: "答复"}}}
	s, boss, _ := startAttrEnv(t, fake)
	sendFrom(t, s, "stub-boss", protocol.MessagePayload{Text: "说一句", Conversation: "conv-deliver-auth"})
	select {
	case <-boss.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("超时：未交付")
	}
	from := boss.LastFrom()
	if from.Kind != "agent" || from.ID != "reasoner" || from.Authority != spv2.AuthorityRuntime {
		t.Fatalf("deliver 的 from = %+v，want agent:reasoner/runtime", from)
	}
}
