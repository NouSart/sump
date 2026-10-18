package tests

import (
	"testing"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// TestConversationActions 验证会话表在服务层的接线（不只是 store 的算术）：
//   - 落消息之后会话自动出现（谁都不用显式建）；
//   - 入口取 boss（记录是 reasoner 代写的，"从哪扇门进来"由它替谁办事决定）；
//   - conversation_list / conversation_rename 两个动作通，且改名对不存在的会话报错。
func TestConversationActions(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 空库先列一次：要的是空列表而不是错误（门户冷启动就是这么调的）。
	var empty protocol.ConversationListResult
	rp := caller.call(t, srv.WsURL(), "conversation_list", protocol.ConversationListPayload{})
	if !rp.OK {
		t.Fatalf("空库 conversation_list 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &empty); err != nil {
		t.Fatal(err)
	}
	if len(empty.Conversations) != 0 {
		t.Fatalf("空库不该有会话: %+v", empty.Conversations)
	}

	rp = caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
		ConversationID: "conv-a", Boss: "frontend", Role: "user", Content: "从门户进来的第一句",
	})
	if !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}

	var list protocol.ConversationListResult
	rp = caller.call(t, srv.WsURL(), "conversation_list", protocol.ConversationListPayload{})
	if !rp.OK {
		t.Fatalf("conversation_list 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Conversations) != 1 {
		t.Fatalf("落一条消息后应有一个会话: %+v", list.Conversations)
	}
	if got := list.Conversations[0]; got.ID != "conv-a" || got.Entry != "frontend" || got.Status != "active" {
		t.Fatalf("会话内容不符: %+v", got)
	}

	var renamed protocol.ConversationResult
	rp = caller.call(t, srv.WsURL(), "conversation_rename", protocol.ConversationRenamePayload{ID: "conv-a", Title: "  门户调样式  "})
	if !rp.OK {
		t.Fatalf("conversation_rename 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &renamed); err != nil {
		t.Fatal(err)
	}
	if renamed.Conversation.Title != "门户调样式" {
		t.Fatalf("标题应去掉首尾空白: %q", renamed.Conversation.Title)
	}

	// 不存在的会话：明确报错，不悄悄造一个（否则门户一个手滑就多出一条空会话）。
	rp = caller.call(t, srv.WsURL(), "conversation_rename", protocol.ConversationRenamePayload{ID: "nope", Title: "x"})
	if rp.OK {
		t.Fatal("给不存在的会话改名应失败")
	}
}
