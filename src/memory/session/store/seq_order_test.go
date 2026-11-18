// seq 排序的直接测试。
//
// reasoner 用 `Fire` 并发写记忆（每个 store 各自成 goroutine），所以**落库顺序（id）
// 与真实先后（seq）会错位**。`History`/`Recent` 的排序必须按 seq，不能按 id。
//
// 之前的测试里 seq 与 id 恰好同序，于是把 `ORDER BY seq DESC, id DESC` 改成
// `ORDER BY id DESC` 也照样绿 —— 这条契约等于从未被验证过。
package store

import (
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 先写 seq=2、再写 seq=1：落库顺序与 seq 顺序相反，读出来必须按 seq。
func TestHistoryOrdersBySeqNotInsertOrder(t *testing.T) {
	s := openTemp(t)

	// 后写的 seq 更小 —— 模拟「先派发的先记，但网络乱序到达」。
	if err := s.Record("c1", "assistant", "后到的（seq=2）", "assistant", "", 2, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("c1", "user", "先发的（seq=1）", "user", "", 1, ""); err != nil {
		t.Fatal(err)
	}

	entries, err := s.History("c1", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应有 2 条记录，实际 %d", len(entries))
	}
	if entries[0].Seq != 1 || entries[1].Seq != 2 {
		t.Fatalf("应按 seq 正序返回，实际 seq = %d, %d", entries[0].Seq, entries[1].Seq)
	}
	if entries[0].Content != "先发的（seq=1）" || entries[1].Content != "后到的（seq=2）" {
		t.Fatalf("内容顺序不符: %q, %q", entries[0].Content, entries[1].Content)
	}
}

// Recent 同样按 seq 取「最近 N 条」，且返回正序。
func TestRecentOrdersBySeqNotInsertOrder(t *testing.T) {
	s := openTemp(t)

	if err := s.Record("c2", "assistant", "第三条", "assistant", "", 3, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("c2", "user", "第一条", "user", "", 1, ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("c2", "user", "第二条", "user", "", 2, ""); err != nil {
		t.Fatal(err)
	}

	msgs, err := s.Recent("c2", 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 2 {
		t.Fatalf("应取最近 2 条，实际 %d", len(msgs))
	}
	if msgs[0].Content != "第二条" || msgs[1].Content != "第三条" {
		t.Fatalf("应取 seq 最大的两条且正序返回，实际 %q, %q", msgs[0].Content, msgs[1].Content)
	}
}

// 重复的对话消息只留一条（上一行完全相同才跳过）；事件类重复必须保留。
func TestRecordDedupesOnlyDialogue(t *testing.T) {
	s := openTemp(t)

	for i := 0; i < 2; i++ {
		if err := s.Record("c3", "user", "同一句话", "user", "", 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := s.History("c3", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("重复对话应只留一条，实际 %d 条", len(entries))
	}

	for i := 0; i < 2; i++ {
		if err := s.Record("c3", "tool", "同一条命令", "tool_result", "", 0, ""); err != nil {
			t.Fatal(err)
		}
	}
	entries, err = s.History("c3", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("工具轨迹不该去重（连跑两次是两件事），实际 %d 条", len(entries))
	}
}
