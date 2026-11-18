package tests

// 角色化部署：session / experience 独立进程，memory 门面唯一对外。
// 这个用例验证门面两条链路都真的跨进程：写入 session、写入 experience，
// 再由 context 两跳拼装。

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
)

func startRole(t *testing.T, role server.Role, center string, st *store.Store, sh *shared.Store, peers map[string]string) *server.Server {
	t.Helper()
	name := string(role)
	if role == server.RoleFacade {
		name = "memory"
	}
	cfg := server.Config{
		Name:              name,
		Role:              role,
		Peers:             peers,
		Listen:            "127.0.0.1:0",
		Center:            center,
		HeartbeatInterval: 200 * time.Millisecond,
		Store:             st,
		Shared:            sh,
		HistoryLimit:      12,
	}
	srv := server.New(cfg, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func TestFacadeRoutesAcrossProcesses(t *testing.T) {
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
		"session":    sess.WsURL(),
		"experience": exp.WsURL(),
	})
	caller := startStubCaller(t, sc.URL())
	if _, ok := sc.cards["session"]; ok {
		t.Fatal("session 不得注册到全局名册")
	}
	if _, ok := sc.cards["experience"]; ok {
		t.Fatal("experience 不得注册到全局名册")
	}
	if card, ok := sc.cards["memory"]; !ok || card.Name != "memory" {
		t.Fatalf("全局名册应只有 memory: %+v", sc.cards)
	}

	if rp := caller.call(t, facade.WsURL(), "store", protocol.StorePayload{
		ConversationID: "t1", Role: "user", Content: "上一轮的问题",
	}); !rp.OK {
		t.Fatalf("store 透传失败: %s", rp.Error)
	}
	if rp := caller.call(t, facade.WsURL(), "remember", protocol.RememberPayload{
		Kind: "偏好", Content: "用户喜欢简洁回答", Priority: 1,
	}); !rp.OK {
		t.Fatalf("remember 透传失败: %s", rp.Error)
	}
	rp := caller.contextUntilReady(t, facade.WsURL(), protocol.ContextPayload{
		Conversation: "t1",
		Boss:         "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "新一轮问题"},
		},
	})
	got := decodeContext(t, rp)
	if got.Task.ID == "" || got.Work.ID == "" {
		t.Fatalf("context 应带任务与 work: %+v", got)
	}
	if len(got.Messages) < 2 || !containsAll(got.Messages[1].Content, "上一轮的问题", "用户喜欢简洁回答") {
		t.Fatalf("记忆块应同时含历史与核心记忆: %+v", got.Messages)
	}

	// 设置下发必须真的转给 experience：核心记忆预算压到 1 字后不再注入。
	if rp := caller.call(t, facade.WsURL(), "configure", protocol.ConfigurePayload{
		Key: "context.core_max_chars", Value: "1",
	}); !rp.OK {
		t.Fatalf("configure 转发失败: %s", rp.Error)
	}
	got = decodeContext(t, caller.contextUntilReady(t, facade.WsURL(), protocol.ContextPayload{
		Conversation: "t1",
		Boss:         "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "再问一次"},
		},
	}))
	if len(got.Messages) < 2 || strings.Contains(got.Messages[1].Content, "用户喜欢简洁回答") {
		t.Fatalf("核心记忆预算压到 1 字后不应再注入: %+v", got.Messages)
	}
}

func TestConsolidateAcrossProcesses(t *testing.T) {
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
	reasoner := startStubReasoner(t, sc.URL())
	exp := startRole(t, server.RoleExperience, "", nil, sh, map[string]string{
		"session": sess.WsURL(), "reasoner": reasoner.URL(),
	})
	facade := startRole(t, server.RoleFacade, sc.URL(), nil, nil, map[string]string{
		"session": sess.WsURL(), "experience": exp.WsURL(),
	})
	caller := startStubCaller(t, sc.URL())

	if rp := caller.call(t, facade.WsURL(), "store", protocol.StorePayload{
		ConversationID: "c1", Role: "user", Content: "我住在杭州",
	}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
	items, err := st.PendingStaged(0)
	if err != nil || len(items) != 1 {
		t.Fatalf("应有 1 条待整理候选: %+v err=%v", items, err)
	}
	reasoner.setText(fmt.Sprintf(`[{"id":%d,"keep":true,"kind":"身份","content":"住在杭州","salience":0.7,"priority":0,"durability":"durable","reason":"稳定事实"}]`, items[0].ID))

	var res protocol.ConsolidateResult
	rp := caller.call(t, facade.WsURL(), "consolidate", struct{}{})
	if !rp.OK {
		t.Fatalf("consolidate 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.Processed != 1 || res.Archived != 1 {
		t.Fatalf("整理结果不符: %+v", res)
	}
	entries, err := sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if strings.Contains(e.Content, "住在杭州") {
			found = true
		}
	}
	if !found {
		t.Fatalf("整理结果应写入 experience 长期记忆: %+v", entries)
	}
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}
