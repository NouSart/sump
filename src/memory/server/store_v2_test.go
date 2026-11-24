package server

// store 的 v2 路径（client_item_id 幂等 + 补报 receipt）的行为测试（S-001 §3.0 / §4.3 / §12）。
//
// 直接打 s.store（共享实现），不经过 SPv2 帧：错误码语义在这一层由 spv2.ErrCode 决定。
// 线上入口（native.go）当前会把错误码统一改写成 invalid_argument，见 store_v2.go 顶部说明。

import (
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func newStoreV2TestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}, st
}

func storeV2Body(t *testing.T, p protocol.StorePayload) []byte {
	t.Helper()
	body, err := protocol.EncodePayload(p)
	if err != nil {
		t.Fatalf("编码 store 载荷: %v", err)
	}
	return body
}

// mustStoreResult 调 s.store 并断言回的是 protocol.StoreResult。
func mustStoreResult(t *testing.T, s *Server, body []byte) protocol.StoreResult {
	t.Helper()
	out, err := s.store(body)
	if err != nil {
		t.Fatalf("store 失败: %v", err)
	}
	res, ok := out.(protocol.StoreResult)
	if !ok {
		t.Fatalf("store 回执类型应为 protocol.StoreResult，实际 %T", out)
	}
	return res
}

func countStoreV2Rows(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数查询失败（%s）: %v", query, err)
	}
	return n
}

func storeV2ReportData(t *testing.T) []byte {
	t.Helper()
	data, err := protocol.EncodePayload(map[string]any{
		"exit_code": int64(0),
		"stdout":    "ok",
		"nested":    map[string]any{"k": "v"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// 同键同载荷两次：第二次 deduped=true、回同一个 item_id / seq，库里只有一行。
func TestStoreV2DedupesReplay(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-1",
	}
	body := storeV2Body(t, p)

	res1 := mustStoreResult(t, s, body)
	if res1.Deduped || res1.ItemID == "" || res1.Seq != 1 {
		t.Fatalf("首次入账应非重放且回 item_id/seq=1，实际 %+v", res1)
	}
	res2 := mustStoreResult(t, s, body)
	if !res2.Deduped {
		t.Fatalf("同键同载荷重放应 deduped=true，实际 %+v", res2)
	}
	if res2.ItemID != res1.ItemID || res2.Seq != res1.Seq {
		t.Fatalf("重放回执应指向同一行：%+v != %+v", res2, res1)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages WHERE conversation_id = 'c1'`); n != 1 {
		t.Fatalf("重放不应新增消息，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts WHERE work = 'w1'`); n != 1 {
		t.Fatalf("重放不应新增幂等记录，实际 %d 行", n)
	}
}

// 同键不同 content：conflict（不是覆盖、也不是新写一行）。
func TestStoreV2ConflictsOnChangedContent(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "user", Kind: "user", Content: "原话",
		ClientItemID: "frontend:item-1",
	}
	mustStoreResult(t, s, storeV2Body(t, p))

	p.Content = "换了一句话"
	_, err := s.store(storeV2Body(t, p))
	if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("同键不同载荷应回 conflict，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages WHERE conversation_id = 'c1'`); n != 1 {
		t.Fatalf("冲突不应写入新消息，实际 %d 行", n)
	}
}

// meta 键序 / 空白 / 数字写法不同但语义相同 → 同一个 payload_hash，判重放而不是 conflict。
func TestStoreV2MetaKeyOrderSameHash(t *testing.T) {
	s, _ := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "assistant", Kind: "assistant", Content: "答复",
		ClientItemID: "frontend:item-2", Meta: `{"a":1,"b":{"x":"y"}}`,
	}
	res1 := mustStoreResult(t, s, storeV2Body(t, p))
	if res1.Deduped {
		t.Fatalf("首次入账不应 deduped: %+v", res1)
	}
	p.Meta = `{ "b" : { "x" : "y" }, "a" : 1.0 }`
	res2 := mustStoreResult(t, s, storeV2Body(t, p))
	if !res2.Deduped || res2.ItemID != res1.ItemID {
		t.Fatalf("meta 键序 / 数字写法不同但语义相同应判重放，实际 %+v（首次 %+v）", res2, res1)
	}
}

// 空 meta 与空对象视为同一种「没有附加信息」，不能因此 conflict。
func TestStoreV2EmptyMetaEquivalent(t *testing.T) {
	s, _ := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-3", Meta: "{}",
	}
	res1 := mustStoreResult(t, s, storeV2Body(t, p))
	p.Meta = ""
	res2 := mustStoreResult(t, s, storeV2Body(t, p))
	if !res2.Deduped || res2.ItemID != res1.ItemID {
		t.Fatalf("空 meta 与 {} 语义相同，应判重放，实际 %+v", res2)
	}
}

// 补报路径：同 report_id 同载荷重放 → deduped，report_receipts 只一行。
func TestStoreV2ReportReplayDedupes(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "tool", Kind: "tool_result", Content: "命令完成",
		ClientItemID: "report:shell:r1",
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: "call-1",
			Phase: "done", Final: true, Data: storeV2ReportData(t),
		},
	}
	res1 := mustStoreResult(t, s, storeV2Body(t, p))
	if res1.Deduped {
		t.Fatalf("首次补报不应 deduped: %+v", res1)
	}
	res2 := mustStoreResult(t, s, storeV2Body(t, p))
	if !res2.Deduped || res2.ItemID != res1.ItemID || res2.Seq != res1.Seq {
		t.Fatalf("同 report_id 同载荷重放应 deduped 且指向同一行，实际 %+v（首次 %+v）", res2, res1)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("补报重放不应新增行，实际 %d 行", n)
	}
}

// 即使换了 client_item_id，同一 report_id + 同 hash 也由 report_receipts 判重放（不重复入账）。
func TestStoreV2ReportReceiptDedupesAcrossClientKeys(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "tool", Kind: "tool_result", Content: "命令完成",
		ClientItemID: "report:shell:r1",
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: "call-1", Phase: "done",
		},
	}
	mustStoreResult(t, s, storeV2Body(t, p))

	p.ClientItemID = "shell:replay-r1" // 不符合 §3.0 规则 5 的键，但补报层面仍应去重
	res := mustStoreResult(t, s, storeV2Body(t, p))
	if !res.Deduped {
		t.Fatalf("同 report_id 同 hash 换 client_item_id 也应 deduped（补报层去重），实际 %+v", res)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("补报层去重失败，report_receipts %d 行", n)
	}
}

// 同 report_id 不同载荷 → conflict（§6.2：交人工介入，不许悄悄覆盖）。
func TestStoreV2ReportConflictOnChangedPayload(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "tool", Kind: "tool_result", Content: "命令完成",
		ClientItemID: "report:shell:r1",
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: "call-1", Phase: "done",
		},
	}
	mustStoreResult(t, s, storeV2Body(t, p))

	// 换 client_item_id 让 store_receipts 不去重，把冲突落到 report_receipts 的 payload_hash 上。
	p.ClientItemID = "shell:replay-r1"
	p.Content = "换了个结果"
	_, err := s.store(storeV2Body(t, p))
	if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("同 report_id 不同载荷应回 conflict，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("冲突不应新增补报记录，实际 %d 行", n)
	}
}

// 同 call 同 report_seq 换 report_id → conflict（只防 report_id 防不住换 ID 重放）。
func TestStoreV2ReportSeqReuseConflicts(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "tool", Kind: "tool_result", Content: "命令完成",
		ClientItemID: "report:shell:r1",
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: "call-1", Phase: "done",
		},
	}
	mustStoreResult(t, s, storeV2Body(t, p))

	p.ClientItemID = "report:shell:r2"
	p.Report.ReportID = "r2"
	p.Report.Phase = "failed"
	_, err := s.store(storeV2Body(t, p))
	if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("同 call 同 report_seq 不同 report_id 应回 conflict，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("冲突不应新增补报记录，实际 %d 行", n)
	}
}

// 非法 meta（坏 JSON / 重复键 / 非对象）→ invalid_argument，且不落任何行。
func TestStoreV2RejectsInvalidMeta(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	for _, meta := range []string{
		`{oops`,
		`{"a":1,"a":2}`, // 重复键：JCS 必须拒绝，不能猜哪个生效
		`[1,2]`,         // meta 的契约是对象
		`"just a string"`,
	} {
		p := protocol.StorePayload{
			ConversationID: "c1", Work: "w1", Role: "user", Kind: "user", Content: "你好",
			ClientItemID: "frontend:item-meta", Meta: meta,
		}
		_, err := s.store(storeV2Body(t, p))
		if !spv2.IsCode(err, spv2.CodeInvalidArgument) {
			t.Fatalf("meta=%q 应回 invalid_argument，实际 %v", meta, err)
		}
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("非法 meta 不应落消息，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts`); n != 0 {
		t.Fatalf("非法 meta 不应写幂等记录，实际 %d 行", n)
	}
}

// work 为空时用 task_id 兜底；两者都空 → invalid_argument（消息必须归属执行单元）。
func TestStoreV2WorkFallbackAndRequired(t *testing.T) {
	s, st := newStoreV2TestServer(t)

	_, err := s.store(storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-1",
	}))
	if !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("work 与 task_id 都空应回 invalid_argument，实际 %v", err)
	}

	// task_id 兜底：receipt 的 work 维度记 task_id。
	res := mustStoreResult(t, s, storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", TaskID: "task-1", Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-1",
	}))
	if res.ItemID == "" {
		t.Fatalf("task_id 兜底路径应正常入账: %+v", res)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts WHERE work = 'task-1'`); n != 1 {
		t.Fatalf("receipt 的 work 应记为 task_id 兜底值，实际 %d 行", n)
	}

	// visibility 只接受契约词汇。
	_, err = s.store(storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-2", Visibility: "world",
	}))
	if !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非法 visibility 应回 invalid_argument，实际 %v", err)
	}
}

// 崩溃窗口（§5.4）：消息与 store_receipts 已提交、report_receipts 还没写就崩了；
// 重放时除了回 deduped 回执，还要把补报面补齐（不能只靠「在 store receipt 处短路」）。
func TestStoreV2ReplayCompletesMissingReportReceipt(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "tool", Kind: "tool_result", Content: "命令完成",
		ClientItemID: "report:shell:r1",
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: "call-1", Phase: "done",
		},
	}
	body := storeV2Body(t, p)
	mustStoreResult(t, s, body)

	// 模拟「写完 store receipt 就崩」：补报面缺失。
	if _, err := st.DB().Exec(`DELETE FROM report_receipts`); err != nil {
		t.Fatal(err)
	}
	res := mustStoreResult(t, s, body)
	if !res.Deduped {
		t.Fatalf("重放应 deduped: %+v", res)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("重放应补齐缺失的 report_receipts，实际 %d 行", n)
	}
}

// 不同 client_item_id 的顺序入账：seq 递增（取号仍由会话内计数器决定）。
func TestStoreV2SeqIncrements(t *testing.T) {
	s, _ := newStoreV2TestServer(t)
	res1 := mustStoreResult(t, s, storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "user", Kind: "user", Content: "第一句",
		ClientItemID: "frontend:item-1",
	}))
	res2 := mustStoreResult(t, s, storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", Work: "w1", Role: "assistant", Kind: "assistant", Content: "第二句",
		ClientItemID: "frontend:item-2",
	}))
	if res1.Seq != 1 || res2.Seq != 2 {
		t.Fatalf("seq 应递增，实际 %d, %d", res1.Seq, res2.Seq)
	}
}

// 旧调用方（无 client_item_id）行为不变：照旧落库、照旧连续相同对话去重、不写 receipt，
// 回执是空的 protocol.StoreResult。
func TestStoreLegacyWithoutClientItemIDUnchanged(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{ConversationID: "c1", Role: "user", Content: "老调用方"}
	body := storeV2Body(t, p)

	res := mustStoreResult(t, s, body)
	if res.ItemID != "" || res.Seq != 0 || res.Deduped {
		t.Fatalf("旧路径应回空 StoreResult，实际 %+v", res)
	}
	mustStoreResult(t, s, body) // 旧规则：连续完全相同 → 跳过，不报错

	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages WHERE conversation_id = 'c1'`); n != 1 {
		t.Fatalf("旧路径连续相同消息应只留一行，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts`); n != 0 {
		t.Fatalf("旧路径不应写 store_receipts，实际 %d 行", n)
	}

	// 空正文：旧行为是直接回空回执、不落库。
	out, err := s.store(storeV2Body(t, protocol.StorePayload{ConversationID: "c1", Role: "user"}))
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := out.(protocol.StoreResult); !ok {
		t.Fatalf("空正文也应回 protocol.StoreResult，实际 %T", out)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages WHERE conversation_id = 'c1'`); n != 1 {
		t.Fatalf("空正文不应落库，实际 %d 行", n)
	}
}

// payload_hash 的直接契约：meta 键序无关；report 出现与否改变 hash；report 全字段参与 hash。
func TestStorePayloadHashContract(t *testing.T) {
	base := protocol.StorePayload{
		Work: "w1", Role: "tool", Kind: "tool_result", Content: "结果",
		Meta: `{"a":1,"b":2}`,
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: "call-1", Phase: "done",
		},
	}
	h1, err := storePayloadHash(&base, "work")
	if err != nil {
		t.Fatal(err)
	}
	// 键序 / 空白 / 数字写法不同 → 同一 hash。
	reordered := base
	reordered.Meta = `{ "b" : 2, "a" : 1.0 }`
	h2, err := storePayloadHash(&reordered, "work")
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("meta 键序不同应得同一 hash：%s != %s", h1, h2)
	}
	// report 的每个字段都参与 hash。
	for _, mutate := range []func(*protocol.StoreReport){
		func(r *protocol.StoreReport) { r.SourceService = "timer" },
		func(r *protocol.StoreReport) { r.ReportID = "r2" },
		func(r *protocol.StoreReport) { r.ReportSeq = 2 },
		func(r *protocol.StoreReport) { r.CallID = "call-2" },
		func(r *protocol.StoreReport) { r.Phase = "failed" },
		func(r *protocol.StoreReport) { r.Final = true },
		func(r *protocol.StoreReport) {
			r.Data, _ = protocol.EncodePayload(map[string]any{"stdout": "changed"})
		},
	} {
		p := base
		rep := *base.Report
		p.Report = &rep
		mutate(p.Report)
		h, err := storePayloadHash(&p, "work")
		if err != nil {
			t.Fatal(err)
		}
		if h == h1 {
			t.Fatalf("report 字段变化后 hash 不应相同（%+v）", *p.Report)
		}
	}
	// 无 report 时 hash 不同（无 report 时省略该键）。
	noReport := base
	noReport.Report = nil
	h3, err := storePayloadHash(&noReport, "work")
	if err != nil {
		t.Fatal(err)
	}
	if h3 == h1 {
		t.Fatal("有无 report 的载荷 hash 不应相同")
	}
}
