// store_receipts / report_receipts 的直接契约测试（S-001 §3.0 规则 3、§4.3、§6.2、§12）：
//
//   - 同键同 hash = 重放（不新增行，回既有 item_id）；
//   - 同键不同 hash = ErrPayloadConflict；
//   - report 路径除了 report_id 还要防「同 call 同 seq 换 report_id 重放」（uq_report_seq）；
//   - RecordV2 要回 item_id 与真实 seq，且既有去重语义不变。
package store

import (
	"context"
	"errors"
	"testing"
)

// countRows 是这些用例的计数工具：幂等断言最终都要落成「库里只有一行」。
func countRows(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数查询失败（%s）: %v", query, err)
	}
	return n
}

func TestPutStoreReceiptDedupesSamePayload(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	existing, deduped, err := s.PutStoreReceipt(ctx, "w1", "frontend:item-1", "hash-a", "42")
	if err != nil {
		t.Fatal(err)
	}
	if existing != "" || deduped {
		t.Fatalf("首次写入应插入且非重放，实际 existing=%q deduped=%v", existing, deduped)
	}

	existing, deduped, err = s.PutStoreReceipt(ctx, "w1", "frontend:item-1", "hash-a", "")
	if err != nil {
		t.Fatal(err)
	}
	if !deduped || existing != "42" {
		t.Fatalf("同键同 hash 应判重放并回既有 item_id，实际 existing=%q deduped=%v", existing, deduped)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM store_receipts WHERE work = 'w1'`); n != 1 {
		t.Fatalf("重放不应新增行，实际 %d 行", n)
	}
}

func TestPutStoreReceiptConflictsOnDifferentPayload(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, _, err := s.PutStoreReceipt(ctx, "w1", "k1", "hash-a", "1"); err != nil {
		t.Fatal(err)
	}
	_, deduped, err := s.PutStoreReceipt(ctx, "w1", "k1", "hash-b", "")
	if !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("同键不同 hash 应回 ErrPayloadConflict，实际 err=%v（deduped=%v）", err, deduped)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM store_receipts WHERE work = 'w1'`); n != 1 {
		t.Fatalf("冲突不应改写既有行，实际 %d 行", n)
	}
}

// 两阶段用法：先占位（itemID 为空）→ 落库后回填 → 后续重放拿到回填后的 item_id。
// 占位行也覆盖「占位后崩溃」的恢复路径：同 hash 时会继续落消息而不是判重放。
func TestPutStoreReceiptTwoPhaseFill(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	existing, deduped, err := s.PutStoreReceipt(ctx, "w1", "k1", "hash-a", "")
	if err != nil || existing != "" || deduped {
		t.Fatalf("占位写入应成功且非重放，实际 existing=%q deduped=%v err=%v", existing, deduped, err)
	}
	// 崩溃恢复：占位行 item_id 还是空，继续落消息时不能再判重放。
	existing, deduped, err = s.PutStoreReceipt(ctx, "w1", "k1", "hash-a", "")
	if err != nil || existing != "" || deduped {
		t.Fatalf("占位未回填时应继续（不判重放），实际 existing=%q deduped=%v err=%v", existing, deduped, err)
	}
	// 回填真实 item_id。
	existing, deduped, err = s.PutStoreReceipt(ctx, "w1", "k1", "hash-a", "99")
	if err != nil || existing != "" || deduped {
		t.Fatalf("回填应成功且非重放，实际 existing=%q deduped=%v err=%v", existing, deduped, err)
	}
	// 回填之后的重放：回既有 item_id。
	existing, deduped, err = s.PutStoreReceipt(ctx, "w1", "k1", "hash-a", "")
	if err != nil || !deduped || existing != "99" {
		t.Fatalf("回填后重放应回 99，实际 existing=%q deduped=%v err=%v", existing, deduped, err)
	}
}

func TestPutReportReceiptDedupesSamePayload(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	deduped, err := s.PutReportReceipt(ctx, "w1", "shell", "r1", "hash-1", "item-1", "call-1", 1)
	if err != nil || deduped {
		t.Fatalf("首次补报应插入且非重放，实际 deduped=%v err=%v", deduped, err)
	}
	deduped, err = s.PutReportReceipt(ctx, "w1", "shell", "r1", "hash-1", "item-1", "call-1", 1)
	if err != nil || !deduped {
		t.Fatalf("同 report_id 同 hash 重放应 deduped，实际 deduped=%v err=%v", deduped, err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("重放不应新增行，实际 %d 行", n)
	}
}

func TestPutReportReceiptConflictsOnDifferentPayload(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.PutReportReceipt(ctx, "w1", "shell", "r1", "hash-1", "item-1", "call-1", 1); err != nil {
		t.Fatal(err)
	}
	// 同 report_id 换了内容（hash 变化）→ conflict，交人工处置（§6.2）。
	if _, err := s.PutReportReceipt(ctx, "w1", "shell", "r1", "hash-2", "item-1", "call-2", 2); !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("同 report_id 不同 hash 应回 ErrPayloadConflict，实际 err=%v", err)
	}
}

// 换 report_id 重放同一个 (call_id, report_seq)：只防 report_id 防不住，靠 uq_report_seq。
func TestPutReportReceiptRejectsReportSeqReuse(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.PutReportReceipt(ctx, "w1", "shell", "r1", "hash-1", "item-1", "call-1", 1); err != nil {
		t.Fatal(err)
	}
	_, err := s.PutReportReceipt(ctx, "w1", "shell", "r2", "hash-2", "item-2", "call-1", 1)
	if !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("同 call 同 report_seq 不同 report_id 应回 ErrPayloadConflict，实际 err=%v", err)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("被唯一索引拦下的补报不应入账，实际 %d 行", n)
	}
}

// 不同 call 的相同 seq 不冲突（seq 只在单个 call 生命周期内有意义）。
func TestPutReportReceiptSeqIsScopedToCall(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.PutReportReceipt(ctx, "w1", "shell", "r1", "hash-1", "item-1", "call-1", 1); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PutReportReceipt(ctx, "w1", "shell", "r2", "hash-2", "item-2", "call-2", 1); err != nil {
		t.Fatalf("不同 call 的 seq=1 不应冲突: %v", err)
	}
}

func TestMessageByItemIDReturnsSeq(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	itemID, seq, deduped, err := s.RecordV2("c1", "user", "你好", "user", "", 0, "", "w1", "work")
	if err != nil {
		t.Fatal(err)
	}
	if deduped || itemID == "" || seq == 0 {
		t.Fatalf("RecordV2 应返回非空 item_id 与真实 seq，实际 itemID=%q seq=%d deduped=%v", itemID, seq, deduped)
	}
	got, err := s.MessageByItemID(ctx, itemID)
	if err != nil {
		t.Fatal(err)
	}
	if got != seq {
		t.Fatalf("MessageByItemID(%q) = %d, want %d", itemID, got, seq)
	}
	if _, err := s.MessageByItemID(ctx, "999999"); err == nil {
		t.Fatal("不存在的 item_id 应返回错误")
	}
}

// RecordV2 的 seq 必须递增（seq=0 时由会话内最大值 +1 补齐），并显式写入 work_id / visibility。
func TestRecordV2ReturnsIncreasingSeqAndWritesWork(t *testing.T) {
	s := openTemp(t)
	itemID1, seq1, _, err := s.RecordV2("c1", "user", "第一句", "user", "", 0, "", "w1", "work")
	if err != nil {
		t.Fatal(err)
	}
	_, seq2, _, err := s.RecordV2("c1", "assistant", "第二句", "assistant", "", 0, "", "w1", "thread")
	if err != nil {
		t.Fatal(err)
	}
	if seq1 != 1 || seq2 != 2 {
		t.Fatalf("seq 应从 1 递增，实际 %d, %d", seq1, seq2)
	}
	// 显式给了 seq 就按给的值落库，回执回的是真实写入值。
	_, seq3, _, err := s.RecordV2("c1", "tool", "命令", "tool_result", "", 7, "", "w1", "task")
	if err != nil {
		t.Fatal(err)
	}
	if seq3 != 7 {
		t.Fatalf("显式 seq=7 应原样落库，实际 %d", seq3)
	}

	var workID, visibility string
	var id int64
	if err := s.db.QueryRow(`SELECT id, work_id, visibility FROM messages WHERE content = '第二句'`).
		Scan(&id, &workID, &visibility); err != nil {
		t.Fatal(err)
	}
	if workID != "w1" || visibility != "thread" {
		t.Fatalf("work_id / visibility 未按 v2 契约落库: work_id=%q visibility=%q", workID, visibility)
	}
	if itemID1 == "" {
		t.Fatal("第一条记录也应回 item_id")
	}
}

// 旧的「连续完全相同的 user/assistant 消息压成一条」语义在 RecordV2 下保留：
// 第二次返回 dedupedMsg=true 且 item_id / seq 指向被保留的那一行。
func TestRecordV2KeepsLegacyDialogueDedup(t *testing.T) {
	s := openTemp(t)
	itemID1, seq1, deduped1, err := s.RecordV2("c1", "user", "同一句", "user", "", 0, "", "w1", "work")
	if err != nil {
		t.Fatal(err)
	}
	itemID2, seq2, deduped2, err := s.RecordV2("c1", "user", "同一句", "user", "", 0, "", "w1", "work")
	if err != nil {
		t.Fatal(err)
	}
	if deduped1 || !deduped2 {
		t.Fatalf("首次应落库、第二次应判旧规则去重，实际 %v / %v", deduped1, deduped2)
	}
	if itemID2 != itemID1 || seq2 != seq1 {
		t.Fatalf("去重后应指向被保留的行：(%q,%d) != (%q,%d)", itemID2, seq2, itemID1, seq1)
	}
	if n := countRows(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("重复对话应只留一行，实际 %d 行", n)
	}
}
