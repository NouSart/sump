package facade_test

// 假 experience 替换：门面只按 SPv2 契约工作，session 与门面代码零改动。
// 这里连 experience 包都不 import，证明边界不是“同一进程里的函数调用”。

import (
	"context"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/facade"
	"github.com/NouSart/sump/memory/session"
	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func startFakeExperience(t *testing.T, settings []protocol.Setting) string {
	t.Helper()
	methods := []spv2.Method{{Name: "settings_schema", Shape: spv2.ShapeCall}, {Name: "inject", Shape: spv2.ShapeCall}}
	for _, name := range []string{"remember", "forget", "consolidate", "note", "dig",
		"lineheads", "prune", "publish_session", "shared_sessions", "shared_forget"} {
		methods = append(methods, spv2.Method{Name: name, Shape: spv2.ShapeCall})
	}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "experience", Actor: spv2.ServiceRef("experience"),
		Version: "fake/1", Methods: methods, Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv.Handle("settings_schema", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(settings)
	})
	srv.Handle("inject", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(protocol.InjectResult{})
	})
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)
	return "ws" + strings.TrimPrefix(hs.URL, "http") + "/spv2"
}

func TestFacadeWorksWithFakeExperiencePeer(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sess := session.New(session.Config{
		Name: "session", Listen: "127.0.0.1:0", Standalone: true,
		Store: st, HistoryLimit: 12, DBPath: "test-session.db",
	}, log.New(io.Discard, "", 0))
	start(t, sess)
	expURL := startFakeExperience(t, []protocol.Setting{
		{Key: "context.core_limit", Default: "3"},
		{Key: "context.core_max_chars", Default: "400"},
		{Key: "context.rel_limit", Default: "5"},
		{Key: "context.rel_max_chars", Default: "800"},
		{Key: "context.embedding", Default: "embedding"},
		{Key: "share.sessions_limit", Default: "5"},
		{Key: "share.digest_chars", Default: "120"},
		{Key: "shared_db", Default: "test-experience.db", ReadOnly: true},
	})
	f := facade.New(facade.Config{
		Name: "memory", Listen: "127.0.0.1:0", Standalone: true,
		Peers:  map[string]string{"session": sess.WsURL(), "experience": expURL},
		Logger: log.New(io.Discard, "", 0),
	}, nil)
	start(t, f)

	opened := openWork(t, f, "reasoner", "w-fake", "c-fake", "只有会话")
	stored := storeEntry(t, f, "reasoner", "w-fake", "c-fake", opened.TaskID, "user", "只有会话", "reasoner:fake")
	out := getContext(t, f, "reasoner", "w-fake", protocol.Trigger{Kind: "user", Item: stored.ItemID})
	if out.Task.ID == "" {
		t.Fatalf("假 experience 下仍应有会话任务: %+v", out)
	}
}
