package tests

// 硬门：内部服务挂掉后，门面不得假装成功。

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
)

func TestFacadeFailsWhenSessionDown(t *testing.T) {
	sc := startStubCenter(t)
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sh, err := shared.Open(filepath.Join(dir, "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	sess := startRole(t, server.RoleSession, "", st, nil, nil)
	exp := startRole(t, server.RoleExperience, "", nil, sh, nil)
	facade := startRole(t, server.RoleFacade, sc.URL(), nil, nil, map[string]string{
		"session": sess.WsURL(), "experience": exp.WsURL(),
	})
	caller := startStubCaller(t, sc.URL())
	sess.Shutdown()

	if rp := caller.call(t, facade.WsURL(), "store", protocol.StorePayload{
		ConversationID: "t1", Role: "user", Content: "写不进去",
	}); rp.OK {
		t.Fatal("session 已挂，store 不得成功")
	}
	if rp := caller.call(t, facade.WsURL(), "context", protocol.ContextPayload{
		Conversation: "t1",
		Messages:     []protocol.Message{{Role: "user", Content: "还有上下文吗"}},
	}); rp.OK {
		t.Fatal("session 已挂，context 不得成功")
	}
}
