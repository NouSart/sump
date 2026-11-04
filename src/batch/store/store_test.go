package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func twoItems() []protocol.BatchItem {
	return []protocol.BatchItem{
		{Input: []byte("first-raw-input")},
		{Input: []byte("second-raw-input"), Key: true, Tag: "关键"},
	}
}

var pol = Policy{Concurrency: 2, TimeoutMS: 30000, OnError: protocol.BatchOnErrorContinue, WakeMode: protocol.BatchWakeAll}

// rawInput 直读 input 列——这里就得绕过 Items()，因为要验的正是"库里还留着没有"。
func rawInput(t *testing.T, s *Store, itemID string, seq int) []byte {
	t.Helper()
	var v []byte
	if err := s.db.QueryRow(`SELECT ifnull(input, x'') FROM items WHERE item_id = ? AND seq = ?`,
		itemID, seq).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// TestMarkItemClearsInput：项一终结，原始 input 必须从库里消失（只存摘要）。
func TestMarkItemClearsInput(t *testing.T) {
	s := newStore(t)
	if err := s.CreateJob("j1", "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if got := rawInput(t, s, "j1", 0); string(got) != "first-raw-input" {
		t.Fatalf("受理后应留着 input（重启恢复要用），实际 %q", got)
	}
	if err := s.MarkItem("j1", Item{Seq: 0, Status: protocol.BatchItemOK, Preview: []byte(`{"n":1}`)}); err != nil {
		t.Fatal(err)
	}
	if got := rawInput(t, s, "j1", 0); len(got) != 0 {
		t.Fatalf("项已终结，input 必须抹掉，实际 %q", got)
	}
	if got := rawInput(t, s, "j1", 1); len(got) == 0 {
		t.Fatal("没终结的项不该被牵连")
	}
}

// TestSkipPendingClearsInput：跳过的项同样不留 input。
func TestSkipPendingClearsInput(t *testing.T) {
	s := newStore(t)
	if err := s.CreateJob("j1", "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), time.Now()); err != nil {
		t.Fatal(err)
	}
	n, err := s.SkipPending("j1")
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("应跳过 2 项，实际 %d", n)
	}
	for seq := 0; seq < 2; seq++ {
		if got := rawInput(t, s, "j1", seq); len(got) != 0 {
			t.Fatalf("seq=%d 已跳过却还留着 input: %q", seq, got)
		}
	}
}

// TestPruneOnlyTerminalAndOld：只清终态且超过 TTL 的任务。
func TestPruneOnlyTerminalAndOld(t *testing.T) {
	s := newStore(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour)
	for _, id := range []string{"old-done", "fresh-done", "old-running"} {
		if err := s.CreateJob(id, "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), old); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.Finish("old-done", protocol.BatchPhaseDone, 2, 0, 0, "完成", old); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish("fresh-done", protocol.BatchPhaseDone, 2, 0, 0, "完成", now); err != nil {
		t.Fatal(err)
	}
	n, err := s.Prune(now, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("只该清理 1 个过期任务，实际 %d", n)
	}
	if _, ok, _ := s.Job("old-done"); ok {
		t.Fatal("过期任务应查不到")
	}
	if _, ok, _ := s.Job("fresh-done"); !ok {
		t.Fatal("TTL 内的任务不该被清")
	}
	if _, ok, _ := s.Job("old-running"); !ok {
		t.Fatal("没结束的任务再老也不能清")
	}
}

// TestKeyDoneRoundTrip：KeyDone 落库能读回来（任务结束后 status 靠它）。
func TestKeyDoneRoundTrip(t *testing.T) {
	s := newStore(t)
	if err := s.CreateJob("j1", "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := s.SetKeyDone("j1", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkItem("j1", Item{Seq: 1, Status: protocol.BatchItemOK}); err != nil {
		t.Fatal(err)
	}
	if err := s.Finish("j1", protocol.BatchPhaseDone, 2, 0, 0, "完成", time.Now()); err != nil {
		t.Fatal(err)
	}
	j, ok, err := s.Job("j1")
	if err != nil || !ok {
		t.Fatalf("读任务失败: ok=%v err=%v", ok, err)
	}
	if j.KeyTotal != 1 || j.KeyDone != 1 {
		t.Fatalf("关键项应为 1/1，实际 %d/%d", j.KeyDone, j.KeyTotal)
	}
}

// TestPolicyRoundTrip：声明的策略必须能从账本读回来（恢复任务照原样接着跑）。
func TestPolicyRoundTrip(t *testing.T) {
	s := newStore(t)
	pol := Policy{Concurrency: 3, TimeoutMS: 1234, OnError: protocol.BatchOnErrorStop, WakeMode: protocol.BatchWakeKey}
	if err := s.CreateJob("j1", "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), time.Now()); err != nil {
		t.Fatal(err)
	}
	j, ok, err := s.Job("j1")
	if err != nil || !ok {
		t.Fatalf("读任务失败: ok=%v err=%v", ok, err)
	}
	got := Policy{Concurrency: j.Concurrency, TimeoutMS: j.TimeoutMS, OnError: j.OnError, WakeMode: j.WakeMode}
	if got != pol {
		t.Fatalf("策略没落下来: 期望 %+v，实际 %+v", pol, got)
	}
}

// TestMarkInterruptedClearsInput：中断项同样是终态，不留原始输入。
func TestMarkInterruptedClearsInput(t *testing.T) {
	s := newStore(t)
	if err := s.CreateJob("j1", "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = s.SetItemStatus("j1", 0, protocol.BatchItemRunning)
	n, err := s.MarkInterrupted("j1", "重启中断")
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("应有 1 项被标中断，实际 %d", n)
	}
	if got := rawInput(t, s, "j1", 0); len(got) != 0 {
		t.Fatalf("中断项不该留 input: %q", got)
	}
}

// TestItemsStatusFilter：明细按状态过滤，matched 是过滤后的总数。
func TestItemsStatusFilter(t *testing.T) {
	s := newStore(t)
	if err := s.CreateJob("j1", "worker", "ok", "caller", "boss", "", "async", pol, twoItems(), time.Now()); err != nil {
		t.Fatal(err)
	}
	_ = s.MarkItem("j1", Item{Seq: 0, Status: protocol.BatchItemOK})
	_ = s.MarkItem("j1", Item{Seq: 1, Status: protocol.BatchItemFailed, Error: "炸了"})

	all, matched, err := s.Items("j1", "", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if matched != 2 || len(all) != 2 {
		t.Fatalf("应返回 2 项，实际 matched=%d len=%d", matched, len(all))
	}
	failed, matched, err := s.Items("j1", protocol.BatchItemFailed, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if matched != 1 || len(failed) != 1 || failed[0].Seq != 1 || failed[0].Error != "炸了" {
		t.Fatalf("失败过滤不符: matched=%d %+v", matched, failed)
	}
}
