package session

// 会话服务的真进程级测试：真 WS + 真 SQLite，不依赖设置中心、不依赖外网。
// 覆盖：store→history 回路、context_base 的授权与正常路径、整理池回执、
// 名片只含本域 method、独立模式（空 Center）可启动。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// startService 起一个独立模式（不连设置中心、Center 留空）的会话服务。
func startService(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	db := filepath.Join(t.TempDir(), "memory.db")
	st, err := store.Open(db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := New(Config{
		Listen:            "127.0.0.1:0",
		Standalone:        true,
		Store:             st,
		DBPath:            db,
		HeartbeatInterval: 200 * time.Millisecond,
	}, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv, st
}

// dial 建立一条到 /spv2 的连接（调用方身份由各用例自己给）。
func dial(t *testing.T, url string) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// call 发一跳并交回回执 body；失败回执以 *spv2.Error 返回，交给用例自己判断。
func call(t *testing.T, c *spv2.Client, from spv2.Ref, work, method string, body any) ([]byte, error) {
	t.Helper()
	f, err := spv2.NewCall(work, from, spv2.ServiceRef("session"), method, body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// TestStoreThenHistory 钉住会话库的读写回路：store 落库 → history 原样带回。
func TestStoreThenHistory(t *testing.T) {
	srv, _ := startService(t)
	c := dial(t, srv.WsURL())

	if _, err := call(t, c, spv2.ServiceRef("reasoner"), "trace-store", "store", protocol.StorePayload{
		ConversationID: "c-1", Role: "user", Content: "把这一步记下来",
	}); err != nil {
		t.Fatalf("store 失败: %v", err)
	}
	body, err := call(t, c, spv2.ServiceRef("portal"), "trace-history", "history", protocol.HistoryPayload{ConversationID: "c-1"})
	if err != nil {
		t.Fatalf("history 失败: %v", err)
	}
	var res protocol.HistoryResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.ConversationID != "c-1" || len(res.Messages) != 1 {
		t.Fatalf("history 回执不符: %+v", res)
	}
	if m := res.Messages[0]; m.Content != "把这一步记下来" || m.Role != "user" || m.At == "" {
		t.Fatalf("history 应原样带回记录与落库时间: %+v", m)
	}
}

// TestContextBase 钉住 context_base 的两条路径：正常取原材料（任务 + work + 历史）
// 与越权（别人登记的 work 不能拿来要上下文）。
func TestContextBase(t *testing.T) {
	srv, _ := startService(t)
	c := dial(t, srv.WsURL())
	payload := protocol.ContextPayload{
		Conversation: "c-ctx",
		Messages:     []protocol.Message{{Role: "user", Content: "把这件事做完"}},
	}

	body, err := call(t, c, spv2.AgentRef("agent-a"), "w-ctx", "context_base", payload)
	if err != nil {
		t.Fatalf("context_base 正常路径失败: %v", err)
	}
	var res contextBaseResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Task.ConversationID != "c-ctx" || res.Task.Name != "把这件事做完" {
		t.Fatalf("任务应由第一条用户消息就地建出: %+v", res.Task)
	}
	if res.Work.ID != "w-ctx" || res.Work.Agent.ID != "agent-a" {
		t.Fatalf("work 应以调用方为 agent 登记: %+v", res.Work)
	}

	// 换一个调用方用同一条 work：不是它的 agent/owner/entry，必须拒绝。
	if _, err := call(t, c, spv2.AgentRef("agent-b"), "w-ctx", "context_base", payload); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("越权应回 permission_denied，实际: %v", err)
	}
}

// TestStagedReceipt 钉住整理池回执：item 走 protocol.StagedItem，
// mark / retry 直接落到会话私有库。
func TestStagedReceipt(t *testing.T) {
	srv, st := startService(t)
	c := dial(t, srv.WsURL())
	if _, err := st.Stage("c-stage", "user", "值得过一遍的观察"); err != nil {
		t.Fatal(err)
	}

	body, err := call(t, c, spv2.ServiceRef("experience"), "trace-stage", "staged_pending", map[string]any{"limit": 10})
	if err != nil {
		t.Fatalf("staged_pending 失败: %v", err)
	}
	var res protocol.StagedPendingResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 || res.Items[0].Content != "值得过一遍的观察" || res.Items[0].Status != "pending" {
		t.Fatalf("staged_pending 回执不符: %+v", res)
	}
	id := res.Items[0].ID

	if _, err := call(t, c, spv2.ServiceRef("experience"), "trace-stage", "staged_mark", map[string]any{
		"id": id, "status": "done",
	}); err != nil {
		t.Fatalf("staged_mark 失败: %v", err)
	}
	// retry 记一次失败：未到上限应留在 pending（done 被拉回待处理）。
	if _, err := call(t, c, spv2.ServiceRef("experience"), "trace-stage", "staged_retry", map[string]any{
		"id": id, "last_error": "模型超时",
	}); err != nil {
		t.Fatalf("staged_retry 失败: %v", err)
	}
	pending, done, _, _, err := st.StagingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if pending != 1 || done != 0 {
		t.Fatalf("整理池状态不符: pending=%d done=%d", pending, done)
	}
}

// TestCardOnlySessionDomain 钉住名片：只有本域 method（没有 experience 动作、
// 没有 context），对外 method 都不作为模型工具暴露；设置项也只声明本域两个键。
func TestCardOnlySessionDomain(t *testing.T) {
	srv, _ := startService(t)
	c := dial(t, srv.WsURL())

	f, err := spv2.NewCall("trace-card", spv2.ServiceRef("test"), spv2.ServiceRef("session"), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		t.Fatalf("hello 失败: %v", err)
	}
	var hello spv2.HelloResponse
	if err := protocol.DecodeRaw(resp.Body, &hello); err != nil {
		t.Fatal(err)
	}

	want := []string{
		// 对外 method（provides 声明的 14 个）
		"store", "history", "conversation_list", "conversation_rename",
		"conversation_open", "conversation_archive",
		"task_current", "task_open", "task_finish", "task_resume", "task_rename", "task_list",
		"context_v2", "trigger_complete",
		// 内部 method（注册但不进 provides）
		"work_get", "lease_charge", "lease_release", "work_update", "work_sweep",
		"work_cancel", "work_renew", "message_post", "message_claim", "message_ack", "message_nack",
		"context_base", "staged_pending", "staged_mark", "staged_retry", "settings_schema",
		// 骨架自带
		"configure", "hello", "health", "metrics",
	}
	allowed := make(map[string]bool, len(want))
	for _, name := range want {
		allowed[name] = true
	}
	got := make(map[string]bool, len(hello.Card.Methods))
	for _, m := range hello.Card.Methods {
		got[m.Name] = true
		if m.Tool {
			t.Fatalf("%s 不应作为模型工具暴露", m.Name)
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("名片缺少本域 method %s; 实际=%v", name, got)
		}
	}
	for name := range got {
		if !allowed[name] {
			t.Fatalf("名片出现非本域 method %s", name)
		}
	}

	// 设置项只声明本域：历史条数可调 + 库路径只读。
	body, err := call(t, c, spv2.ServiceRef("memory"), "trace-settings", "settings_schema", map[string]any{})
	if err != nil {
		t.Fatalf("settings_schema 失败: %v", err)
	}
	var settings []protocol.Setting
	if err := protocol.DecodeRaw(body, &settings); err != nil {
		t.Fatal(err)
	}
	if len(settings) != 2 || settings[0].Key != "context.history_limit" || settings[1].Key != "db" {
		t.Fatalf("设置项应只声明本域两个键: %+v", settings)
	}
	if !settings[1].ReadOnly {
		t.Fatalf("db 必须是只读项: %+v", settings[1])
	}
}

// TestStandaloneStartsWithEmptyCenter 独立模式（不连设置中心、Center 为空）也能启动，
// 且默认值对齐：服务名 session、历史条数 12。
func TestStandaloneStartsWithEmptyCenter(t *testing.T) {
	srv, _ := startService(t)
	if srv.WsURL() == "" {
		t.Fatal("独立模式启动后应已监听 /spv2")
	}
	if srv.cfg.Name != "session" {
		t.Fatalf("默认服务名应为 session，实际 %q", srv.cfg.Name)
	}
	if v := srv.SettingValue("context.history_limit"); v != "12" {
		t.Fatalf("默认历史条数应为 12，实际 %q", v)
	}
	// 没有名册也要能应答：真实一跳能回，才说明 /spv2 起来了。
	body, err := call(t, dial(t, srv.WsURL()), spv2.ServiceRef("test"), "trace-standalone",
		"conversation_list", protocol.ConversationListPayload{})
	if err != nil {
		t.Fatalf("独立模式下应答失败: %v", err)
	}
	var res protocol.ConversationListResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
}
