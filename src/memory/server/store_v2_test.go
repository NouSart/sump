package server

// store 的 v2 路径（client_item_id 幂等 + 补报 receipt）的行为测试（S-001 §3.0 / §4.3 / §12）。
//
// 直接打 s.store（共享实现），不经过 SPv2 帧：错误码语义在这一层由 spv2.ErrCode 决定。
// 线上入口（native.go）把 body 与帧头一起交给它，所以这里的调用也照实带上调用方 work 与 from。
//
// 每一处幂等断言都同时看四件事：messages 行数、store_receipts 行数、report_receipts 行数、
// calls 状态。审计（R-012）抓到的三类缺陷全是「回执说没有、消息却写进去了」，
// 只数回执等于什么都没验。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// v2 测试用的固定身份：reasoner 同时服务 w1 / w2 两条 work（红队场景的前提）。
const (
	v2CallerWork = "w1"
	v2Intruder   = "w2"
)

func v2Agent() spv2.Ref           { return spv2.AgentRef("reasoner") }
func v2Owner(who string) spv2.Ref { return spv2.HumanRef(who) }

func newStoreV2TestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	// work 是 v2 的归属单位：没有登记的 work 就没有可授权的调用（memory 是唯一登记处）。
	for _, w := range []spv2.Work{
		{ID: v2CallerWork, Root: v2CallerWork, Class: spv2.ClassRequest,
			Owner: spv2.HumanRef("alice"), Agent: spv2.AgentRef("reasoner"),
			Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning},
		{ID: v2Intruder, Root: v2Intruder, Class: spv2.ClassRequest,
			Owner: spv2.HumanRef("bob"), Agent: spv2.AgentRef("reasoner"),
			Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning},
	} {
		if _, err := st.EnsureWork(ctx, w); err != nil {
			t.Fatalf("登记 work %s: %v", w.ID, err)
		}
	}
	return &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}, st
}

// seedV2Call 按 v2 契约先开一条 call（补报必须引用真实存在的 call_id）。
func seedV2Call(t *testing.T, st *store.Store, work, service string) string {
	t.Helper()
	id, err := st.CreateCall(context.Background(), work, "", "", "", service, "run",
		store.CallModeOnce, false, 0, 0)
	if err != nil {
		t.Fatalf("CreateCall(%s/%s): %v", work, service, err)
	}
	return id
}

func storeV2Body(t *testing.T, p protocol.StorePayload) []byte {
	t.Helper()
	body, err := protocol.EncodePayload(p)
	if err != nil {
		t.Fatalf("编码 store 载荷: %v", err)
	}
	return body
}

// storeV2 以默认调用方（w1 的 agent）打一次 store。
func storeV2(t *testing.T, s *Server, body []byte) (protocol.StoreResult, error) {
	t.Helper()
	out, err := s.store(body, v2CallerWork, v2Agent())
	if err != nil {
		return protocol.StoreResult{}, err
	}
	res, ok := out.(protocol.StoreResult)
	if !ok {
		t.Fatalf("store 回执类型应为 protocol.StoreResult，实际 %T", out)
	}
	return res, nil
}

// mustStoreResult 调 s.store 并断言成功。
func mustStoreResult(t *testing.T, s *Server, body []byte) protocol.StoreResult {
	t.Helper()
	res, err := storeV2(t, s, body)
	if err != nil {
		t.Fatalf("store 失败: %v", err)
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

// reportPayload 造一条合法补报：终态必须 final=true（§3.0 规则 4 的词汇约束）。
func reportPayload(work, key, callID, content string) protocol.StorePayload {
	return protocol.StorePayload{
		ConversationID: "c1", Work: work, Role: "tool", Kind: "tool_result", Content: content,
		ClientItemID: key,
		Report: &protocol.StoreReport{
			SourceService: "shell", ReportID: "r1", ReportSeq: 1, CallID: callID,
			Phase: "done", Final: true,
		},
	}
}

// 同键同载荷两次：第二次 deduped=true、回同一个 item_id / seq，库里只有一行。
func TestStoreV2DedupesReplay(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "你好",
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
		ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "原话",
		ClientItemID: "frontend:item-1",
	}
	mustStoreResult(t, s, storeV2Body(t, p))

	p.Content = "换了一句话"
	_, err := storeV2(t, s, storeV2Body(t, p))
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
		ConversationID: "c1", Work: v2CallerWork, Role: "assistant", Kind: "assistant", Content: "答复",
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
		ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "你好",
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
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	p.Report.Data = storeV2ReportData(t)
	body := storeV2Body(t, p)

	res1 := mustStoreResult(t, s, body)
	if res1.Deduped {
		t.Fatalf("首次补报不应 deduped: %+v", res1)
	}
	res2 := mustStoreResult(t, s, body)
	if !res2.Deduped || res2.ItemID != res1.ItemID || res2.Seq != res1.Seq {
		t.Fatalf("同 report_id 同载荷重放应 deduped 且指向同一行，实际 %+v（首次 %+v）", res2, res1)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("补报重放不应新增行，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("补报重放不应新增消息，实际 %d 行", n)
	}
}

// 即使换了 client_item_id，同一 report_id + 同 hash 也由 report_receipts 判重放（不重复入账）。
func TestStoreV2ReportReceiptDedupesAcrossClientKeys(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	mustStoreResult(t, s, storeV2Body(t, p))

	p.ClientItemID = "shell:replay-r1" // 不符合 §3.0 规则 5 的键，但补报层面仍应去重
	res := mustStoreResult(t, s, storeV2Body(t, p))
	if !res.Deduped {
		t.Fatalf("同 report_id 同 hash 换 client_item_id 也应 deduped（补报层去重），实际 %+v", res)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("补报层去重失败，report_receipts %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("补报层去重失败，messages %d 行", n)
	}
}

// 同 report_id 不同载荷 → conflict（§6.2：交人工介入，不许悄悄覆盖），且零写入。
func TestStoreV2ReportConflictOnChangedPayload(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	mustStoreResult(t, s, storeV2Body(t, p))

	// 换 client_item_id 让 store_receipts 不去重，把冲突落到 report_receipts 的 payload_hash 上。
	p.ClientItemID = "shell:replay-r1"
	p.Content = "换了个结果"
	_, err := storeV2(t, s, storeV2Body(t, p))
	if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("同 report_id 不同载荷应回 conflict，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("冲突不应新增补报记录，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("冲突不应新增消息，实际 %d 行", n)
	}
}

// 同 call 同 report_seq 换 report_id → conflict（只防 report_id 防不住换 ID 重放）。
func TestStoreV2ReportSeqReuseConflicts(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	mustStoreResult(t, s, storeV2Body(t, p))

	p.ClientItemID = "report:shell:r2"
	p.Report.ReportID = "r2"
	p.Content = "同一次调用的另一种说法"
	_, err := storeV2(t, s, storeV2Body(t, p))
	if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("同 call 同 report_seq 不同 report_id 应回 conflict，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("冲突不应新增补报记录，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("冲突不应新增消息，实际 %d 行", n)
	}
}

// 补报必须驱动调用账本：结果先落库、同一事务里关闭（S-001 §5.1）。
func TestStoreV2ReportClosesCall(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	ctx := context.Background()
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	res := mustStoreResult(t, s, storeV2Body(t, p))

	row, err := st.GetCall(ctx, callID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Observe != store.CallObserveClosed || row.Exec != store.CallExecDone ||
		row.Phase != store.CallPhaseDone || row.ResultRef != res.ItemID {
		t.Fatalf("done 补报应关闭调用并指向本次消息，实际 %+v", row)
	}
}

// 非终态补报只推进执行维：不写 phase、不关闭（结果还没来）。
func TestStoreV2ReportAcceptedKeepsCallOpen(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	ctx := context.Background()
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "已开始执行")
	p.Report.Phase = "accepted"
	p.Report.Final = false
	mustStoreResult(t, s, storeV2Body(t, p))

	row, err := st.GetCall(ctx, callID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("accepted 补报应只推进到 running、不写 phase，实际 %+v", row)
	}
}

// 补报的 phase 与 final 必须自洽：终态不带 final=true 是契约违规。
func TestStoreV2ReportPhaseFinalMismatch(t *testing.T) {
	s, _ := newStoreV2TestServer(t)
	callID := seedV2Call(t, s.cfg.Store, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	p.Report.Final = false
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("phase=done 但 final=false 应回 invalid_argument，实际 %v", err)
	}
	p.Report.Phase = "accepted"
	p.Report.Final = true
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("phase=accepted 却 final=true 应回 invalid_argument，实际 %v", err)
	}
	p.Report.Phase = "succeeded"
	p.Report.Final = true
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("词汇表外的 phase 应回 invalid_argument，实际 %v", err)
	}
}

// 补报引用不存在的 call → not_found，且一条消息都不留（审计抓到的「回执说没有、消息却进去了」）。
func TestStoreV2ReportUnknownCall(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := reportPayload(v2CallerWork, "report:shell:r1", "call-not-exists", "命令完成")
	_, err := storeV2(t, s, storeV2Body(t, p))
	if !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("补报引用不存在的 call 应回 not_found，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("被拒绝的补报不应留下消息，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts`); n != 0 {
		t.Fatalf("被拒绝的补报不应留下幂等记录，实际 %d 行", n)
	}
}

// 补报冒充别的服务：report.source_service 与 call 的 owner 不一致 → conflict。
func TestStoreV2ReportImpersonatesOwner(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	p.Report.SourceService = "timer"
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("冒充别的 owner 应回 conflict，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("被拒绝的补报不应留下消息，实际 %d 行", n)
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
			ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "你好",
			ClientItemID: "frontend:item-meta", Meta: meta,
		}
		if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
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

// work 必填且不接受 task_id 兜底：task 与 work 是两个标识空间（S-001 §2.1）。
func TestStoreV2WorkRequiredNoTaskFallback(t *testing.T) {
	s, st := newStoreV2TestServer(t)

	// 两者都空。
	p := protocol.StorePayload{
		ConversationID: "c1", Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-1",
	}
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("work 为空应回 invalid_argument，实际 %v", err)
	}

	// 只给 task_id：不接受拿 task 冒充 work。
	p.Work = ""
	p.TaskID = "task-1"
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("只有 task_id 应回 invalid_argument（task 不能冒充 work），实际 %v", err)
	}

	// work 没登记过：memory 是唯一 work 登记处，查不到就是 not_found。
	p.Work = "never-registered"
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("未登记的 work 应回 not_found，实际 %v", err)
	}

	// visibility 只接受契约词汇。
	p.Work = v2CallerWork
	p.ClientItemID = "frontend:item-2"
	p.Visibility = "world"
	if _, err := storeV2(t, s, storeV2Body(t, p)); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非法 visibility 应回 invalid_argument，实际 %v", err)
	}

	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts`); n != 0 {
		t.Fatalf("被拒绝的请求不应留下任何回执，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("被拒绝的请求不应留下任何消息，实际 %d 行", n)
	}
}

// 跨 work 写入：reasoner 用 w2 的帧头往 w1 名下写消息，必须拒绝（INV-14）。
func TestStoreV2RejectsCrossWorkWrite(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "alice 的秘密",
		ClientItemID: "frontend:item-1",
	}
	body := storeV2Body(t, p)
	if _, err := s.store(body, v2Intruder, v2Agent()); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("跨 work 写应回 permission_denied，实际 %v", err)
	}
	// owner 权限也要精确对应目标 work 的 owner：bob 不能往 alice 的 work 里写。
	if _, err := s.store(body, "", v2Owner("bob")); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("别的 owner 写应回 permission_denied，实际 %v", err)
	}
	// 同 owner 的 human 入口放行（用户从自己的入口补记历史）。
	if _, err := s.store(body, "", v2Owner("alice")); err != nil {
		t.Fatalf("同 owner 的入口写自己的 work 应放行，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("只有被放行的一次该落库，实际 %d 行", n)
	}
}

// 帧头没有 work 又不是 owner / runtime：一律拒绝，不靠载荷自报的 work 放行。
func TestStoreV2RejectsAnonymousCaller(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{
		ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "你好",
		ClientItemID: "frontend:item-1",
	}
	if _, err := s.store(storeV2Body(t, p), "", spv2.ServiceRef("shell")); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("无帧头 work 的服务调用应回 permission_denied，实际 %v", err)
	}
	// owner 但 owner 名字为空：没有可核对的归属，fail-closed。
	if _, err := s.store(storeV2Body(t, p), "", spv2.Ref{Kind: "human", Authority: spv2.AuthorityOwner}); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("owner 名为空时应 fail-closed，实际 %v", err)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("被拒绝的请求不应留下消息，实际 %d 行", n)
	}
}

// 崩溃窗口（§5.4 / INV-13）：消息、store 回执、补报回执、call 账本同事务提交，
// 所以「回执有、补报面没有」这种半截状态**不可能**由一次正常的 store 产生。
// 用直接改库模拟出来的半截状态，重放必须回既有回执，而不是再补一条消息。
func TestStoreV2ReplayNeverDoubleWrites(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	callID := seedV2Call(t, st, v2CallerWork, "shell")
	p := reportPayload(v2CallerWork, "report:shell:r1", callID, "命令完成")
	body := storeV2Body(t, p)
	first := mustStoreResult(t, s, body)

	// 人为破坏一致性（只有直接改库做得到，正常路径产生不了）：删掉补报面。
	if _, err := st.DB().Exec(`DELETE FROM report_receipts`); err != nil {
		t.Fatal(err)
	}
	res, err := storeV2(t, s, body)
	if err != nil {
		t.Fatalf("重放应回既有回执而不是报错: %v", err)
	}
	if !res.Deduped || res.ItemID != first.ItemID {
		t.Fatalf("重放应回既有回执，实际 %+v（首次 %+v）", res, first)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("重放不得写第二条消息，实际 %d 行", n)
	}
}

// 不同 client_item_id 的顺序入账：seq 递增（取号只由会话内计数器决定，S-001 §4.7）。
func TestStoreV2SeqIncrements(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	res1 := mustStoreResult(t, s, storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", Work: v2CallerWork, Role: "user", Kind: "user", Content: "第一句",
		ClientItemID: "frontend:item-1",
	}))
	res2 := mustStoreResult(t, s, storeV2Body(t, protocol.StorePayload{
		ConversationID: "c1", Work: v2CallerWork, Role: "assistant", Kind: "assistant", Content: "第二句",
		ClientItemID: "frontend:item-2",
		Seq:          999, // 调用方给的序号不作数：序号只由库决定
	}))
	if res1.Seq != 1 || res2.Seq != 2 {
		t.Fatalf("seq 应由计数器递增，实际 %d, %d", res1.Seq, res2.Seq)
	}
	var stored int64
	if err := st.DB().QueryRow(`SELECT seq FROM messages WHERE content = '第二句'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if stored != 2 {
		t.Fatalf("调用方 seq=999 不应生效，实际落库 %d", stored)
	}
	var counter int64
	if err := st.DB().QueryRow(`SELECT last_seq FROM seq_counters WHERE conversation_id = 'c1'`).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if counter != 2 {
		t.Fatalf("seq_counters 应与落库序号一致，实际 %d", counter)
	}
}

// 旧调用方（无 client_item_id）行为不变：照旧落库、照旧连续相同对话去重、不写 receipt，
// 回执是空的 protocol.StoreResult。
func TestStoreLegacyWithoutClientItemIDUnchanged(t *testing.T) {
	s, st := newStoreV2TestServer(t)
	p := protocol.StorePayload{ConversationID: "c1", Role: "user", Content: "老调用方"}
	body := storeV2Body(t, p)

	res, err := storeV2(t, s, body)
	if err != nil {
		t.Fatal(err)
	}
	if res.ItemID != "" || res.Seq != 0 || res.Deduped {
		t.Fatalf("旧路径应回空 StoreResult，实际 %+v", res)
	}
	if _, err := storeV2(t, s, body); err != nil { // 旧规则：连续完全相同 → 跳过，不报错
		t.Fatal(err)
	}

	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM messages WHERE conversation_id = 'c1'`); n != 1 {
		t.Fatalf("旧路径连续相同消息应只留一行，实际 %d 行", n)
	}
	if n := countStoreV2Rows(t, st, `SELECT COUNT(*) FROM store_receipts`); n != 0 {
		t.Fatalf("旧路径不应写 store_receipts，实际 %d 行", n)
	}

	// 空正文：旧行为是直接回空回执、不落库。
	out, err := s.store(storeV2Body(t, protocol.StorePayload{ConversationID: "c1", Role: "user"}), v2CallerWork, v2Agent())
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
