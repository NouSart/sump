package store

import (
	"fmt"
	"sync"
	"testing"
)

// insertMessages 直接落消息行，避开 Record 的去重与会话 touch，
// 用来构造超过默认上限的历史数据。
func insertMessages(t *testing.T, s *Store, conversation string, n int) {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	stmt, err := tx.Prepare(`INSERT INTO messages
		(conversation_id, role, content, kind, meta, seq, created_at, task_id)
		VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		if _, err := stmt.Exec(
			conversation,
			"user",
			fmt.Sprintf("m%03d", i),
			"user",
			"",
			i,
			"2026-01-01T00:00:00Z",
			"",
		); err != nil {
			_ = stmt.Close()
			_ = tx.Rollback()
			t.Fatal(err)
		}
	}
	if err := stmt.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

// limit 再大也不能一次吐出全部历史，否则前端/上下文会被撑爆。
func TestHistoryClampsLimitTo500(t *testing.T) {
	s := openTemp(t)
	insertMessages(t, s, "many", 505)

	got, err := s.History("many", 1000, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 500 {
		t.Fatalf("History(limit=1000) 应夹到 500 条，实际 %d 条", len(got))
	}
	if got[0].Content != "m006" || got[499].Content != "m505" {
		t.Fatalf("夹紧后应保留最近 500 条: 首=%q 尾=%q", got[0].Content, got[499].Content)
	}
}

// Recent 的 limit<=0 是「默认窗口」，不是「全部」。
func TestRecentDefaultWindowIs12(t *testing.T) {
	s := openTemp(t)
	insertMessages(t, s, "recent", 15)

	got, err := s.Recent("recent", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 12 {
		t.Fatalf("Recent(limit=0) 应默认取 12 条，实际 %d 条", len(got))
	}
	if got[0].Content != "m004" || got[11].Content != "m015" {
		t.Fatalf("默认窗口应是最新 12 条: 首=%q 尾=%q", got[0].Content, got[11].Content)
	}
}

// 对话消息的去重必须覆盖 assistant；只压 user 会让重试产生重复答复。
func TestRecordDedupesAssistantDialogue(t *testing.T) {
	s := openTemp(t)

	for i := 1; i <= 2; i++ {
		if err := s.Record("c", "assistant", "同一句答复", "assistant", "", int64(i), ""); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := s.History("c", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 {
		t.Fatalf("连续相同的 assistant 消息应被压成 1 条，实际 %d 条", len(hist))
	}
	if hist[0].Content != "同一句答复" || hist[0].Kind != "assistant" {
		t.Fatalf("去重后内容不对: %+v", hist[0])
	}
}

// 并发落不同消息：一条都不能丢，seq 仍决定最终顺序。
func TestConcurrentRecordsKeepEveryEntry(t *testing.T) {
	s := openTemp(t)

	const n = 32
	errCh := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			if err := s.Record("race", "user", fmt.Sprintf("并发-%02d", i), "user", "", int64(i+1), ""); err != nil {
				errCh <- fmt.Sprintf("第 %d 条 Record 失败: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}

	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("SQLite 连接池应为 1（单写者），实际 %d", got)
	}

	hist, err := s.History("race", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != n {
		t.Fatalf("并发写入后应有 %d 条，实际 %d 条", n, len(hist))
	}
	for i, m := range hist {
		want := int64(i + 1)
		if m.Seq != want {
			t.Fatalf("第 %d 条 seq 应为 %d，实际 %d（顺序错乱）", i, want, m.Seq)
		}
	}
}
