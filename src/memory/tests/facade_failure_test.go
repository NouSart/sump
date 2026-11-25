package tests

// 硬门：内部服务挂掉后，门面不得假装成功。

import (
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestFacadeFailsWhenSessionDown(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())
	srv.sess.Shutdown()

	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
		ConversationID: "t1", Work: "w-down", ClientItemID: "reasoner:down",
		Role: "user", Content: "写不进去",
	}); rp.OK {
		t.Fatal("session 已挂，store 不得成功")
	}
	if rp := caller.call(t, srv.WsURL(), "context", protocol.ContextRequest{
		Work: "w-down", Trigger: protocol.Trigger{Kind: "user", Item: "1"},
	}); rp.OK {
		t.Fatal("session 已挂，context 不得成功")
	}
}

func TestFacadeFailsWhenExperienceDown(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	opened := caller.openWork(t, srv.WsURL(), "w-exp-down", "t2", "经验挂掉")
	msg := caller.storeV2(t, srv.WsURL(), "w-exp-down", "t2", opened.TaskID, "user", "还有记忆吗", "reasoner:exp")
	srv.exp.Shutdown()

	if rp := caller.call(t, srv.WsURL(), "context", protocol.ContextRequest{
		Work: "w-exp-down", Trigger: protocol.Trigger{Kind: "user", Item: msg.ItemID},
	}); rp.OK {
		t.Fatal("experience 已挂，context 不得成功")
	}
}
