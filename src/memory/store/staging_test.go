package store

import (
	"path/filepath"
	"strings"
	"testing"
)

func openStagingStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "staging.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestStagingLifecycle(t *testing.T) {
	st := openStagingStore(t)

	if _, err := st.Stage("c1", "", "   "); err == nil {
		t.Fatal("空内容应拒绝")
	}
	id, err := st.Stage("c1", "user", "  我住在杭州  ")
	if err != nil || id <= 0 {
		t.Fatalf("Stage 失败: id=%d err=%v", id, err)
	}
	items, err := st.PendingStaged(0)
	if err != nil || len(items) != 1 {
		t.Fatalf("PendingStaged 不符: %+v err=%v", items, err)
	}
	it := items[0]
	if it.ID != id || it.ConversationID != "c1" || it.Role != "user" || it.Content != "我住在杭州" || it.Status != "pending" {
		t.Fatalf("候选字段不符: %+v", it)
	}

	if err := st.MarkStaged(id, "bogus", ""); err == nil {
		t.Fatal("非法状态应拒绝")
	}
	if err := st.MarkStaged(id, "done", ""); err != nil {
		t.Fatal(err)
	}
	if items, err := st.PendingStaged(0); err != nil || len(items) != 0 {
		t.Fatalf("done 后不应再 pending: %+v err=%v", items, err)
	}
	pending, done, skipped, failed, err := st.StagingCounts()
	if err != nil || pending != 0 || done != 1 || skipped != 0 || failed != 0 {
		t.Fatalf("计数不符: %d/%d/%d/%d err=%v", pending, done, skipped, failed, err)
	}
}

func TestStagingRetryCapsAtFailed(t *testing.T) {
	st := openStagingStore(t)
	id, err := st.Stage("c1", "user", "一条会一直失败的候选")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < StagingMaxAttempts; i++ {
		if err := st.RetryStaged(id, "模型输出坏了"); err != nil {
			t.Fatal(err)
		}
	}
	pending, _, _, failed, err := st.StagingCounts()
	if err != nil {
		t.Fatal(err)
	}
	if pending != 0 || failed != 1 {
		t.Fatalf("达到重试上限后应标 failed: pending=%d failed=%d", pending, failed)
	}
	items, err := st.PendingStaged(0)
	if err != nil || len(items) != 0 {
		t.Fatalf("failed 不应再被取出: %+v err=%v", items, err)
	}
}

func TestStagingBatchLimitAndOrder(t *testing.T) {
	st := openStagingStore(t)
	for _, text := range []string{"一", "二", "三"} {
		if _, err := st.Stage("c", "user", text); err != nil {
			t.Fatal(err)
		}
	}
	items, err := st.PendingStaged(2)
	if err != nil || len(items) != 2 {
		t.Fatalf("批大小不符: %+v err=%v", items, err)
	}
	if !strings.Contains(items[0].Content, "一") || !strings.Contains(items[1].Content, "二") {
		t.Fatalf("应按 id 升序: %+v", items)
	}
}
