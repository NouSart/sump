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
	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/memory/facade"
	"github.com/NouSart/sump/memory/session"
	"github.com/NouSart/sump/memory/session/store"
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

// openWork 经门面登记 work / 任务（与 reasoner 建链同一条路）。
func openWork(t *testing.T, f *facade.Server, from, work, cid, taskName string) protocol.WorkOpenResult {
	t.Helper()
	var out protocol.WorkOpenResult
	if err := call(t, f.WsURL(), from, work, "work_open", protocol.WorkOpenPayload{
		Work: work, ConversationID: cid, TaskName: taskName, Boss: from,
	}, &out); err != nil {
		t.Fatalf("work_open(%s) 失败: %v", work, err)
	}
	return out
}

// storeEntry 经门面走一次 v2 store，返回 item_id（context 的 trigger.item）。
func storeEntry(t *testing.T, f *facade.Server, from, work, cid, taskID, role, content, key string) protocol.StoreResult {
	t.Helper()
	var out protocol.StoreResult
	if err := call(t, f.WsURL(), from, work, "store", protocol.StorePayload{
		ConversationID: cid, Work: work, TaskID: taskID, ClientItemID: key,
		Role: role, Content: content,
	}, &out); err != nil {
		t.Fatalf("store(%s) 失败: %v", work, err)
	}
	return out
}

// getContext 经门面取一次 context/2（session 基座 + experience 现注入）。
func getContext(t *testing.T, f *facade.Server, from, work string, trigger protocol.Trigger) protocol.ContextResponse {
	t.Helper()
	var out protocol.ContextResponse
	if err := call(t, f.WsURL(), from, work, "context", protocol.ContextRequest{
		Work: work, Trigger: trigger,
	}, &out); err != nil {
		t.Fatalf("context(%s) 失败: %v", work, err)
	}
	return out
}

func TestFacadeContextRoutesAndSettings(t *testing.T) {
	f, _, _ := startStack(t)

	opened := openWork(t, f, "reasoner", "w1", "c1", "上下文任务")
	prev := storeEntry(t, f, "reasoner", "w1", "c1", opened.TaskID, "user", "上一轮", "reasoner:prev")
	if err := call(t, f.WsURL(), "reasoner", "w1", "remember", protocol.RememberPayload{
		Kind: "偏好", Content: "用户喜欢简洁", Priority: 1,
	}, nil); err != nil {
		t.Fatal(err)
	}
	got := getContext(t, f, "reasoner", "w1", protocol.Trigger{Kind: "user", Item: prev.ItemID})
	if got.Task.ID == "" || got.Work.ID != "w1" {
		t.Fatalf("context 应带 task/work: %+v", got)
	}
	joined := ""
	for _, m := range got.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "上一轮") || !strings.Contains(joined, "用户喜欢简洁") {
		t.Fatalf("基座历史与记忆块都应出现在消息链里: %+v", got.Messages)
	}
	if !hasMemoryPrefix(got.Messages) {
		t.Fatalf("experience 注入缺失【记忆】块: %+v", got.Messages)
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

// TestFacadeTranscriptForwards 门面只做转发：transcript 的 work/snapshot_id 原样到达 session，
// 文本来自那份快照（S-001 §3.2）。
func TestFacadeTranscriptForwards(t *testing.T) {
	f, _, _ := startStack(t)
	const workID = "w-transcript"

	// 与 context 用例同一套准备：先 work_open 登记 work / 任务，再走 v2 store 落消息。
	opened := openWork(t, f, "reasoner", workID, "c-transcript", "transcript 转发任务")
	stored := storeEntry(t, f, "reasoner", workID, "c-transcript", opened.TaskID, "user", "门面转发的问题", "reasoner:transcript-1")
	assembled := getContext(t, f, "reasoner", workID,
		protocol.Trigger{Kind: "user", Item: stored.ItemID})

	var got protocol.TranscriptResult
	if err := call(t, f.WsURL(), "reasoner", workID, "transcript",
		protocol.TranscriptPayload{Work: workID, SnapshotID: assembled.SnapshotID}, &got); err != nil {
		t.Fatalf("门面转发 transcript 失败: %v", err)
	}
	if !strings.Contains(got.Text, "门面转发的问题") {
		t.Fatalf("transcript 文本应来自那份快照: %+v", got)
	}
	if got.Truncated || got.Omitted != 0 {
		t.Fatalf("无预算时不应截断: %+v", got)
	}

	// 错误的 snapshot_id 不能被门面吞掉：原样回 session 的 not_found。
	err := call(t, f.WsURL(), "reasoner", workID, "transcript",
		protocol.TranscriptPayload{Work: workID, SnapshotID: "no-such-snapshot"}, nil)
	if !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("缺失快照应 not_found，实际: %v", err)
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

// TestFacadeContextFourStatesAndTriggerComplete 门面组合 context 的四态：
// work_open 登记、context 取基座并注入 experience、trigger_complete 经门面原样到达
// session（含 completed 只补 ACK、不再注入 experience）。
func TestFacadeContextFourStatesAndTriggerComplete(t *testing.T) {
	f, st, _ := startStack(t)
	const workID = "w-v2"

	opened := openWork(t, f, "reasoner", workID, "c-v2", "v2 上下文任务")
	task, ok, err := st.CurrentTask("c-v2")
	if err != nil || !ok {
		t.Fatalf("work_open 后应有进行中任务: %+v, err=%v", task, err)
	}
	if opened.TaskID != task.ID {
		t.Fatalf("work_open 回的 task_id 与当前任务不一致: %q vs %q", opened.TaskID, task.ID)
	}

	// 经门面走 v2 store（走同一条原子路径，带 work 归属与 client_item_id）。
	stored := storeEntry(t, f, "reasoner", workID, "c-v2", task.ID, "user", "第一轮问题", "reasoner:trigger-1")
	req := protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: stored.ItemID}}

	first := getContext(t, f, "reasoner", workID, req.Trigger)
	if first.SnapshotID == "" || first.TriggerState != "assembled" || len(first.Messages) == 0 {
		t.Fatalf("首次组装回执不符: %+v", first)
	}
	if first.Messages[0].Role != "system" || first.Task.ID != task.ID || first.Work.ID != workID {
		t.Fatalf("首次组装投影不符: task=%+v work=%+v messages=%+v", first.Task, first.Work, first.Messages)
	}
	// assembled：同一触发复用同一基座快照（experience 段每次现算，不参与快照）。
	second := getContext(t, f, "reasoner", workID, req.Trigger)
	if second.SnapshotID != first.SnapshotID {
		t.Fatalf("assembled 应复用快照: %q vs %q", second.SnapshotID, first.SnapshotID)
	}

	// trigger_complete：经门面转发且幂等。
	for i := 0; i < 2; i++ {
		var done protocol.TriggerCompleteResult
		if err := call(t, f.WsURL(), "reasoner", workID, "trigger_complete",
			protocol.TriggerCompletePayload{Work: workID, Trigger: req.Trigger}, &done); err != nil {
			t.Fatalf("第 %d 次 trigger_complete 失败: %v", i+1, err)
		}
		if !done.Completed {
			t.Fatalf("trigger_complete 应回 completed=true: %+v", done)
		}
	}

	// completed：快照删掉也只补 ACK（完成事实不随快照消失）。
	if _, err := st.DB().Exec(`DELETE FROM context_snapshots`); err != nil {
		t.Fatal(err)
	}
	var ack protocol.ContextResponse
	if err := call(t, f.WsURL(), "reasoner", workID, "context", req, &ack); err != nil {
		t.Fatalf("completed 后经门面取 context 失败: %v", err)
	}
	if !ack.Completed || ack.TriggerState != "completed" || len(ack.Messages) != 0 {
		t.Fatalf("completed 只应补 ACK: %+v", ack)
	}
	if ack.SnapshotID != first.SnapshotID {
		t.Fatalf("ACK 应指回原快照: %q vs %q", ack.SnapshotID, first.SnapshotID)
	}
}

// hasMemoryPrefix 判断消息链里有没有门面注入的【记忆】块。
func hasMemoryPrefix(msgs []protocol.LLMMessage) bool {
	for _, m := range msgs {
		if m.Role == "system" && strings.HasPrefix(m.Content, "【记忆】") {
			return true
		}
	}
	return false
}
