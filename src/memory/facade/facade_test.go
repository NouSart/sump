package facade_test

// 门面集成：真 session + 真 experience + 门面，全部走 SPv2。
// 只验证门面自己的责任：卡片/设置聚合、路由、context 拼装。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/experience"
	"github.com/NouSart/sump/memory/facade"
	"github.com/NouSart/sump/memory/session"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func startStack(t *testing.T) (*facade.Server, *store.Store, *shared.Store) {
	t.Helper()
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

	sess := session.New(session.Config{
		Name: "session", Listen: "127.0.0.1:0", Standalone: true,
		Store: st, HistoryLimit: 12, DBPath: "test-session.db",
		Logger: log.New(io.Discard, "", 0),
	}, nil)
	start(t, sess)
	exp := experience.New(experience.Config{
		Name: "experience", Listen: "127.0.0.1:0", Standalone: true,
		Shared: sh, SharedPath: "test-experience.db",
		Logger: log.New(io.Discard, "", 0),
	}, nil)
	start(t, exp)
	f := facade.New(facade.Config{
		Name: "memory", Listen: "127.0.0.1:0", Standalone: true,
		Peers:  map[string]string{"session": sess.WsURL(), "experience": exp.WsURL()},
		Logger: log.New(io.Discard, "", 0),
	}, nil)
	start(t, f)
	return f, st, sh
}

func start(t *testing.T, s interface {
	Start(context.Context) error
	Shutdown()
}) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
}

func call(t *testing.T, url, from, work, method string, in, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := spv2.Dial(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if work == "" {
		work = "w-test"
	}
	frame, err := spv2.NewCall(work, spv2.ServiceRef(from), spv2.ServiceRef("memory"), method, in)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := conn.Call(ctx, frame)
	if err != nil {
		return err
	}
	if out != nil {
		return resp.DecodeBody(out)
	}
	return nil
}

func TestFacadeContextRoutesAndSettings(t *testing.T) {
	f, _, _ := startStack(t)

	if err := call(t, f.WsURL(), "reasoner", "w1", "store", protocol.StorePayload{
		ConversationID: "c1", Role: "user", Content: "上一轮",
	}, nil); err != nil {
		t.Fatal(err)
	}
	if err := call(t, f.WsURL(), "reasoner", "w1", "remember", protocol.RememberPayload{
		Kind: "偏好", Content: "用户喜欢简洁", Priority: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	var got struct {
		Messages []protocol.Message `msgpack:"messages"`
		Work     spv2.Work          `msgpack:"work"`
		Task     protocol.Task      `msgpack:"task"`
	}
	if err := call(t, f.WsURL(), "reasoner", "w1", "context", protocol.ContextPayload{
		Conversation: "c1", Boss: "e2e", Messages: []protocol.Message{
			{Role: "system", Content: "sys"}, {Role: "user", Content: "新一轮"},
		},
	}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Task.ID == "" || got.Work.ID == "" {
		t.Fatalf("context 应带 task/work: %+v", got)
	}
	if len(got.Messages) < 2 || !strings.Contains(got.Messages[1].Content, "上一轮") ||
		!strings.Contains(got.Messages[1].Content, "用户喜欢简洁") {
		t.Fatalf("记忆块不符: %+v", got.Messages)
	}

	keys := []string{}
	for _, s := range f.Settings() {
		keys = append(keys, s.Key)
	}
	want := []string{"context.history_limit", "context.core_limit", "context.core_max_chars",
		"context.rel_limit", "context.rel_max_chars", "context.embedding",
		"share.sessions_limit", "share.digest_chars", "db", "shared_db", "name", "addr", "center"}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Fatalf("门面设置顺序不符:\n got %v\nwant %v", keys, want)
	}
}

func TestFacadeRejectsMissingPeer(t *testing.T) {
	f := facade.New(facade.Config{
		Name: "memory", Listen: "127.0.0.1:0", Standalone: true,
		Peers:  map[string]string{},
		Logger: log.New(io.Discard, "", 0),
	}, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := f.Start(ctx); err == nil || !strings.Contains(err.Error(), "session") {
		t.Fatalf("缺 session 应启动失败: %v", err)
	}
}
