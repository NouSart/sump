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
		ConversationID: "t1", Role: "user", Content: "写不进去",
	}); rp.OK {
		t.Fatal("session 已挂，store 不得成功")
	}
	if rp := caller.call(t, srv.WsURL(), "context", protocol.ContextPayload{
		Conversation: "t1",
		Messages:     []protocol.Message{{Role: "user", Content: "还有上下文吗"}},
	}); rp.OK {
		t.Fatal("session 已挂，context 不得成功")
	}
}
