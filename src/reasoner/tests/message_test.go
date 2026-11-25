package tests

// 生产入口验证：human(authority=owner) 的 message 走与 message 相同的 context 主链。

import (
	"context"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func TestHumanMessageOpensTaskViaContext(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "好的"}}}
	s, _, mem := startAttrEnv(t, fake)
	c := dialNative(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call, err := spv2.NewCall("w-msg-human", spv2.HumanRef("owner"), spv2.ServiceRef("reasoner"), "message", protocol.MessagePayload{
		Text: "帮我看看", Conversation: "c-msg-human",
	})
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Status string `msgpack:"status"`
	}
	if err := c.CallDecode(ctx, call, &out); err != nil {
		t.Fatalf("message 未被受理: %v", err)
	}
	if out.Status != "accepted" {
		t.Fatalf("回执不符: %+v", out)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && mem.Recalls() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if mem.Recalls() == 0 {
		t.Fatal("human message 没有走 context 主链")
	}
	if got := mem.RecallContexts(); len(got) == 0 || got[0].ConversationID != "c-msg-human" {
		t.Fatalf("context 会话不对: %+v", got)
	}
	if wo := mem.WorkOpens()["w-msg-human"]; wo.ConversationID != "c-msg-human" || wo.TaskName != "帮我看看" {
		t.Fatalf("work_open 应带上会话与任务名: %+v", wo)
	}
	// 用户消息的 store 回执 item_id 就是 context 的 trigger.item；载荷必须带 work / 幂等键。
	var userStore protocol.StorePayload
	for _, sp := range mem.Stores() {
		if sp.Role == "user" {
			userStore = sp
		}
	}
	if userStore.Work != "w-msg-human" || userStore.ClientItemID == "" {
		t.Fatalf("用户消息 store 应带 work 与非空 client_item_id: %+v", userStore)
	}
}
