package store

// 快照 / 触发状态 / 按 work 查消息的包内测试（S-001 §3.1、§3.1.1、§4.4）。

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"
	"time"

	"github.com/NouSart/sump/spv2"
)

// insertSnapshotMessage 直接往 messages 写一行（work_id / visibility 是 v2 新列，
// Store.Record 不暴露它们，测试里用 SQL 造数据）。
func insertSnapshotMessage(t *testing.T, st *Store, workID, taskID, role, kind, content, meta string, seq int64) string {
	t.Helper()
	res, err := st.db.Exec(
		`INSERT INTO messages(conversation_id, role, content, kind, meta, seq, task_id, work_id, visibility, created_at)
		 VALUES('c-test', ?, ?, ?, ?, ?, ?, ?, 'work', '2026-01-01T00:00:00Z')`,
		role, content, kind, meta, seq, taskID, workID)
	if err != nil {
		t.Fatalf("写入测试消息失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("取测试消息 id 失败: %v", err)
	}
	return strconv.FormatInt(id, 10)
}

// TestTriggerItemExists 触发条目必须已落库，且归属本 work 或旧记录（work_id=”）。
func TestTriggerItemExists(t *testing.T) {
	st := openV2Store(t)
	ctx := context.Background()

	mine := insertSnapshotMessage(t, st, "w1", "t1", "user", "user", "我的消息", "", 1)
	legacy := insertSnapshotMessage(t, st, "", "t1", "user", "user", "旧记录", "", 2)
	other := insertSnapshotMessage(t, st, "w2", "t1", "user", "user", "别人的消息", "", 3)

	for _, tc := range []struct {
		name string
		work string
		item string
		want bool
	}{
		{"本 work 的消息", "w1", mine, true},
		{"旧记录（work_id 为空）对任何 work 可见", "w1", legacy, true},
		{"别的 work 的消息", "w1", other, false},
		{"十进制但不存在", "w1", "999999", false},
		{"不是十进制", "w1", "not-an-id", false},
		{"空 item", "w1", "", false},
	} {
		got, err := st.TriggerItemExists(ctx, tc.work, tc.item)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got != tc.want {
			t.Errorf("%s: TriggerItemExists(%q,%q) = %v，期望 %v", tc.name, tc.work, tc.item, got, tc.want)
		}
	}
}

// TestTriggerStateLifecycle 状态读写的 upsert / 幂等完成 / pending 冲突。
func TestTriggerStateLifecycle(t *testing.T) {
	st := openV2Store(t)
	ctx := context.Background()

	state, snap, err := st.GetTriggerState(ctx, "w1", "user", "item-1")
	if err != nil || state != "" || snap != "" {
		t.Fatalf("不存在的触发状态应为空，实际 (%q,%q,%v)", state, snap, err)
	}

	if err := st.SetTriggerState(ctx, "w1", "user", "item-1", TriggerStatePending, ""); err != nil {
		t.Fatal(err)
	}
	state, snap, err = st.GetTriggerState(ctx, "w1", "user", "item-1")
	if err != nil || state != TriggerStatePending || snap != "" {
		t.Fatalf("pending 状态不对: (%q,%q,%v)", state, snap, err)
	}

	// pending 不能直接完成：必须 conflict（S-001 §3.1.1）。
	if err := st.CompleteTrigger(ctx, "w1", "user", "item-1"); !errors.Is(err, ErrTriggerConflict) {
		t.Fatalf("对 pending 调 CompleteTrigger 应 ErrTriggerConflict，实际: %v", err)
	}
	// 没有状态记录同样是「尚未组装」→ conflict。
	if err := st.CompleteTrigger(ctx, "w1", "user", "never-assembled"); !errors.Is(err, ErrTriggerConflict) {
		t.Fatalf("对不存在的触发调 CompleteTrigger 应 ErrTriggerConflict，实际: %v", err)
	}

	// assembled 可以完成；completed 幂等，且 snapshot_id 保留。
	if err := st.SetTriggerState(ctx, "w1", "user", "item-1", TriggerStateAssembled, "snap-1"); err != nil {
		t.Fatal(err)
	}
	if err := st.CompleteTrigger(ctx, "w1", "user", "item-1"); err != nil {
		t.Fatalf("assembled → completed 失败: %v", err)
	}
	state, snap, err = st.GetTriggerState(ctx, "w1", "user", "item-1")
	if err != nil || state != TriggerStateCompleted || snap != "snap-1" {
		t.Fatalf("completed 状态不对: (%q,%q,%v)", state, snap, err)
	}
	if err := st.CompleteTrigger(ctx, "w1", "user", "item-1"); err != nil {
		t.Fatalf("重复 trigger_complete 应幂等成功，实际: %v", err)
	}
}

// TestSnapshotSaveLoadCompressExpire 快照的压缩存储、冲突、缺失与过期语义。
func TestSnapshotSaveLoadExpire(t *testing.T) {
	st := openV2Store(t)
	ctx := context.Background()
	now := time.Now()
	payload := []byte(`{"schema":"context/2","messages":[]}`)

	if err := st.SaveSnapshot(ctx, "snap-1", "w1", "user", "item-1", payload, "context/2", now, now.Add(time.Hour)); err != nil {
		t.Fatalf("保存快照失败: %v", err)
	}

	// 落盘的 payload 是 gzip：不是明文，且解压回来与原文一致。
	var raw []byte
	if err := st.db.QueryRow(`SELECT payload FROM context_snapshots WHERE snapshot_id = 'snap-1'`).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(raw, payload) {
		t.Error("快照 payload 未压缩")
	}
	if !bytes.HasPrefix(raw, []byte{0x1f, 0x8b}) {
		t.Errorf("快照 payload 不是 gzip 格式: % x", raw[:min(8, len(raw))])
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip 头不合法: %v", err)
	}
	plain, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil || !bytes.Equal(plain, payload) {
		t.Fatalf("解压结果不对: %q err=%v", plain, err)
	}

	// 正常读取：id / 载荷 / 过期时间都对。
	snapID, got, expiresAt, err := st.LoadSnapshot(ctx, "w1", "user", "item-1")
	if err != nil || snapID != "snap-1" || !bytes.Equal(got, payload) {
		t.Fatalf("读取快照不对: (%q,%q,%v) err=%v", snapID, got, expiresAt, err)
	}
	if time.Until(expiresAt) <= 0 {
		t.Fatalf("未过期快照的 expires_at 应在将来: %v", expiresAt)
	}

	// 同一 (work, kind, item) 已有快照 → conflict，原快照不被覆盖。
	err = st.SaveSnapshot(ctx, "snap-2", "w1", "user", "item-1", []byte("新的"), "context/2", now, now.Add(time.Hour))
	if !errors.Is(err, ErrSnapshotConflict) {
		t.Fatalf("重复快照应 ErrSnapshotConflict，实际: %v", err)
	}
	if _, got, _, err := st.LoadSnapshot(ctx, "w1", "user", "item-1"); err != nil || !bytes.Equal(got, payload) {
		t.Fatalf("冲突不应覆盖原快照: %q err=%v", got, err)
	}

	// 缺失 → ErrSnapshotNotFound。
	if _, _, _, err := st.LoadSnapshot(ctx, "w1", "user", "missing"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("缺失快照应 ErrSnapshotNotFound，实际: %v", err)
	}

	// 过期 → ErrSnapshotExpired，且不返回载荷。
	if err := st.SaveSnapshot(ctx, "snap-old", "w1", "tool_result", "item-2", payload, "context/2",
		now.Add(-2*time.Hour), now.Add(-time.Hour)); err != nil {
		t.Fatalf("保存过期快照失败: %v", err)
	}
	snapID, got, _, err = st.LoadSnapshot(ctx, "w1", "tool_result", "item-2")
	if !errors.Is(err, ErrSnapshotExpired) {
		t.Fatalf("过期快照应 ErrSnapshotExpired，实际: %v", err)
	}
	if snapID != "snap-old" || got != nil {
		t.Fatalf("过期快照应返回 id 且不返回载荷，实际 (%q,%q)", snapID, got)
	}
}

// TestRecentMessagesByWork 归属过滤（本 work + 旧记录同 task）、seq 升序、默认条数。
func TestRecentMessagesByWork(t *testing.T) {
	st := openV2Store(t)
	ctx := context.Background()

	insertSnapshotMessage(t, st, "w1", "t1", "user", "user", "第二条", "", 2)
	first := insertSnapshotMessage(t, st, "w1", "t1", "user", "user", "第一条", "", 1)
	legacy := insertSnapshotMessage(t, st, "", "t1", "assistant", "assistant", "旧记录", "", 3)
	insertSnapshotMessage(t, st, "", "t2", "user", "user", "别的任务", "", 4)
	insertSnapshotMessage(t, st, "w2", "t1", "user", "user", "别的 work", "", 5)

	rows, err := st.RecentMessagesByWork(ctx, "w1", "t1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("应取到 3 条（本 work 2 + 旧记录同 task 1），实际 %d: %+v", len(rows), rows)
	}
	wantOrder := []string{"第一条", "第二条", "旧记录"}
	for i, want := range wantOrder {
		if rows[i].Content != want {
			t.Fatalf("第 %d 条 = %q，期望 %q（应按 seq 升序）: %+v", i, rows[i].Content, want, rows)
		}
	}
	if rows[0].ItemID != first || rows[2].ItemID != legacy {
		t.Fatalf("ItemID 应是 messages.id 的十进制串: %+v", rows)
	}
	if rows[2].WorkID != "" || rows[2].Visibility != "work" {
		t.Fatalf("旧记录字段不符: %+v", rows[2])
	}

	// limit 取的是「最近 N 条」（结果仍升序）。
	rows, err = st.RecentMessagesByWork(ctx, "w1", "t1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Content != "第二条" || rows[1].Content != "旧记录" {
		t.Fatalf("limit=2 应取最近两条且升序，实际 %+v", rows)
	}
}

// TestListImageIndex 图片索引按 (owner, conversation) 隔离并按首次看到时间排序。
func TestListImageIndex(t *testing.T) {
	st := openV2Store(t)
	ctx := context.Background()
	insertImage := func(owner, conversation, id, gist, status, seen string) {
		t.Helper()
		if _, err := st.db.Exec(
			`INSERT INTO image_index(owner, conversation_id, image_id, gist, status, first_seen_at, last_viewed_at)
			 VALUES(?,?,?,?,?,?,?)`,
			owner, conversation, id, gist, status, seen, seen); err != nil {
			t.Fatalf("写入图片索引失败: %v", err)
		}
	}
	insertImage("o1", "c1", "img-b", "后看到的", "stored", "2026-01-01T00:00:02Z")
	insertImage("o1", "c1", "img-a", "先看到的", "lost", "2026-01-01T00:00:01Z")
	insertImage("o1", "c2", "img-c", "别的会话", "stored", "2026-01-01T00:00:03Z")
	insertImage("o2", "c1", "img-d", "别人的", "stored", "2026-01-01T00:00:04Z")

	rows, err := st.ListImageIndex(ctx, "o1", "c1")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应只看到本会话的 2 张图，实际 %+v", rows)
	}
	if rows[0].ID != "img-a" || rows[0].Status != "lost" || rows[1].ID != "img-b" {
		t.Fatalf("图片索引顺序 / 字段不对: %+v", rows)
	}
}

// TestTaskIDOfWork work 所属任务读取；work 不存在报 ErrWorkNotFound。
func TestTaskIDOfWork(t *testing.T) {
	st := openV2Store(t)
	ctx := context.Background()
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID: "w1", Root: "w1", Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkOpen,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`UPDATE works SET task = 't1' WHERE id = 'w1'`); err != nil {
		t.Fatal(err)
	}
	taskID, err := st.TaskIDOfWork(ctx, "w1")
	if err != nil || taskID != "t1" {
		t.Fatalf("TaskIDOfWork = (%q,%v)，期望 (t1,nil)", taskID, err)
	}
	if _, err := st.TaskIDOfWork(ctx, "missing"); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("不存在的 work 应 ErrWorkNotFound，实际: %v", err)
	}
}
