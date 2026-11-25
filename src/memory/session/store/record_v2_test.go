// v2 原子落库（RecordV2Atomic）的存储层契约测试（S-001 §3.0 规则 1–6 / §4.3 / §4.7 / §5.1 / INV-13）。
//
// 这一组用例是「实现真的漏了」那一类缺陷的永久回归：审计复现过三个场景——换 client_item_id
// 的补报重放、同 report_id 换内容的冲突、回执没回填的重放——它们共同的问题都是
// 「消息已经写了，幂等判定还没做完」。所以每条断言都同时看四件事：
// messages 行数、store_receipts 行数、report_receipts 行数、calls 状态。
// 只数回执等于什么都没验。
package store

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/NouSart/sump/spv2"
)

const (
	recHashA = "1111111111111111111111111111111111111111111111111111111111111111"
	recHashB = "2222222222222222222222222222222222222222222222222222222222222222"
)

// recInput 造一条最小的 v2 落库请求（会话 c1 / work w1）。
func recInput(key, hash string) V2RecordInput {
	return V2RecordInput{
		ConversationID: "c1", Role: "user", Content: "第一条", Kind: "user",
		WorkID: "w1", Visibility: "work", ClientItemID: key, PayloadHash: hash,
	}
}

// recReport 给一条请求挂上补报元数据（每条补报的正文不同，避免撞上消息层的连续相同去重）。
func recReport(key, hash, source, reportID, callID, phase string, seq int64, final bool) V2RecordInput {
	in := recInput(key, hash)
	in.Content = "补报 " + reportID
	in.Report = &V2ReportInput{
		SourceService: source, ReportID: reportID, ReportSeq: seq,
		CallID: callID, Phase: phase, Final: final, Data: []byte("owner 的结构化补充"),
	}
	return in
}

func countTable(t *testing.T, s *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := s.db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("计数查询失败（%s）: %v", query, err)
	}
	return n
}

// ensureCallsWork 登记一条工作 work（owner 可不同，用来验证补报的 work 归属校验）。
func ensureCallsWork(t *testing.T, st *Store, id, owner string) {
	t.Helper()
	if _, err := st.EnsureWork(context.Background(), spv2.Work{
		ID: id, Root: id, Class: spv2.ClassRequest,
		Owner: spv2.HumanRef(owner), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatalf("登记 work %s: %v", id, err)
	}
}

// createV2Call 直接落一条「已派发前」的 call 行（observe=waiting / exec=created）。
// P1 不搬 call_open 的 handler API，所以这里只造账本起点，不复制它的校验。
func createV2Call(t *testing.T, st *Store, work, service string) string {
	t.Helper()
	seq := countTable(t, st, `SELECT COUNT(*) FROM calls`)
	id := fmt.Sprintf("call-%s-%d", work, seq+1)
	now := "2026-01-01T00:00:00Z"
	if _, err := st.db.Exec(`INSERT INTO calls
		(id, work, service, observe_state, exec_state, created_at, updated_at)
		VALUES(?,?,?,?,?,?,?)`,
		id, work, service, CallObserveWaiting, CallExecCreated, now, now); err != nil {
		t.Fatalf("创建 call %s: %v", id, err)
	}
	return id
}

// callState 读一行 call 的两维状态与 result_ref（P1 不搬 GetCall，测试直接读列）。
func callState(t *testing.T, st *Store, callID string) (observe, exec, phase, resultRef string) {
	t.Helper()
	if err := st.db.QueryRow(
		`SELECT observe_state, exec_state, COALESCE(phase, ''), result_ref FROM calls WHERE id = ?`,
		callID).Scan(&observe, &exec, &phase, &resultRef); err != nil {
		t.Fatalf("读取 call %s: %v", callID, err)
	}
	return observe, exec, phase, resultRef
}

// TestRecordV2AtomicWritesMessageAndReceipt 首次入账：消息 + 回执 + 会话活跃时间一次提交。
func TestRecordV2AtomicWritesMessageAndReceipt(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()

	res, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	if res.ItemID == "" || res.Seq != 1 || res.Deduped {
		t.Fatalf("首次入账应返回新行回执，实际 %+v", res)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("消息应恰好 1 行，实际 %d", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM store_receipts WHERE work='w1' AND client_item_id='k1' AND item_id = ?`, res.ItemID); n != 1 {
		t.Fatalf("store 回执应指向本次消息，实际 %d 行", n)
	}
	var updated string
	if err := s.db.QueryRow(`SELECT updated_at FROM conversations WHERE id='c1'`).Scan(&updated); err != nil {
		t.Fatalf("原子落库必须同时刷新会话行: %v", err)
	}
	if updated == "" {
		t.Fatal("会话活跃时间没有随消息落库")
	}
}

// TestRecordV2AtomicReplaysSameClientKey 同键同 hash = 重放：回既有回执，不新增消息。
func TestRecordV2AtomicReplaysSameClientKey(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	first, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	again, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Deduped || again.ItemID != first.ItemID || again.Seq != first.Seq {
		t.Fatalf("重放应原样回既有回执，实际 first=%+v again=%+v", first, again)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("重放不应新增消息，实际 %d 行", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM store_receipts`); n != 1 {
		t.Fatalf("重放不应新增回执，实际 %d 行", n)
	}
}

// TestRecordV2AtomicConflictLeavesNoMessage 同键不同 hash = conflict，且**不得留下消息**。
//
// 审计复现：实现把「消息先写、回执后判」拆开，冲突时消息已经在库里，ACK 却是错误——
// 调用方以为没入账，历史里却多了一条。这条用例盯的就是这个。
func TestRecordV2AtomicConflictLeavesNoMessage(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA)); err != nil {
		t.Fatal(err)
	}
	changed := recInput("k1", recHashB)
	changed.Content = "换了内容"
	if _, err := s.RecordV2Atomic(ctx, changed); !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("同键不同 hash 应回 ErrPayloadConflict，实际 %v", err)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("冲突不应写入新消息，实际 %d 行", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM store_receipts`); n != 1 {
		t.Fatalf("冲突不应改写既有回执，实际 %d 行", n)
	}
}

// TestRecordV2AtomicPlaceholderBackfill 旧两阶段路径崩在中间留下的占位行：
// 同 hash 时继续落库并**回填**那一行，不再插第二条回执。
func TestRecordV2AtomicPlaceholderBackfill(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if _, err := s.db.Exec(`INSERT INTO store_receipts(work, client_item_id, payload_hash, item_id, created_at)
		VALUES('w1','k1',?,'','2026-01-01T00:00:00Z')`, recHashA); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	if res.Deduped {
		t.Fatal("占位行没有既有回执，不应判重放")
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM store_receipts WHERE item_id = ?`, res.ItemID); n != 1 {
		t.Fatalf("占位行应被回填而不是新增一条，实际匹配 %d 行", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM store_receipts`); n != 1 {
		t.Fatalf("回填不应留下第二行回执，实际 %d 行", n)
	}
	// 之后同键重放：拿到回填后的既有回执。
	again, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	if !again.Deduped || again.ItemID != res.ItemID {
		t.Fatalf("回填后重放应回原 item_id，实际 %+v", again)
	}
}

// TestRecordV2AtomicReportIgnoresClientKey 补报的身份是 report_id：
// 换 client_item_id 重放同一条补报，仍然只入账一次。
func TestRecordV2AtomicReportIgnoresClientKey(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	ensureCallsWork(t, s, "w1", "alice")
	callID := createV2Call(t, s, "w1", "shell")

	in := recReport("outbox-item-1", recHashA, "shell", "r1", callID, "done", 1, true)
	first, err := s.RecordV2Atomic(ctx, in)
	if err != nil {
		t.Fatal(err)
	}
	// 同一条补报换了前端键（例如 owner 重新出一份 outbox 条目）。
	in.ClientItemID = "outbox-item-2"
	again, err := s.RecordV2Atomic(ctx, in)
	if err != nil {
		t.Fatalf("换 client_item_id 的补报重放不应报错: %v", err)
	}
	if !again.Deduped || again.ItemID != first.ItemID || again.Seq != first.Seq {
		t.Fatalf("补报重放必须回既有回执，实际 first=%+v again=%+v", first, again)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("补报重放不得写第二条消息，实际 %d 行", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("补报重放不得写第二条回执，实际 %d 行", n)
	}
}

// TestRecordV2AtomicReportConflictLeavesNoMessage 同 report_id 换内容 = conflict，零写入。
func TestRecordV2AtomicReportConflictLeavesNoMessage(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	ensureCallsWork(t, s, "w1", "alice")
	callID := createV2Call(t, s, "w1", "shell")

	if _, err := s.RecordV2Atomic(ctx, recReport("k1", recHashA, "shell", "r1", callID, "done", 1, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordV2Atomic(ctx, recReport("k2", recHashB, "shell", "r1", callID, "failed", 1, true)); !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("同 report_id 不同载荷应回 ErrPayloadConflict，实际 %v", err)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("冲突不应写入新消息，实际 %d 行", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM report_receipts`); n != 1 {
		t.Fatalf("冲突不应改写既有补报回执，实际 %d 行", n)
	}
}

// TestRecordV2AtomicReportSeqReuseRejected 换 report_id 抢占同一 (call_id, report_seq) = conflict。
func TestRecordV2AtomicReportSeqReuseRejected(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	ensureCallsWork(t, s, "w1", "alice")
	callID := createV2Call(t, s, "w1", "shell")

	if _, err := s.RecordV2Atomic(ctx, recReport("k1", recHashA, "shell", "r1", callID, "done", 1, true)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordV2Atomic(ctx, recReport("k2", recHashB, "shell", "r2", callID, "failed", 1, true)); !errors.Is(err, ErrPayloadConflict) {
		t.Fatalf("同 call 同 report_seq 换 report_id 应回 ErrPayloadConflict，实际 %v", err)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("被拦下的补报不应入账，实际 %d 行", n)
	}
	// 不同 call 用同一个 seq 不冲突（seq 只在单次调用生命周期内有意义）。
	other := createV2Call(t, s, "w1", "shell")
	if _, err := s.RecordV2Atomic(ctx, recReport("k3", recHashB, "shell", "r3", other, "accepted", 1, false)); err != nil {
		t.Fatalf("不同 call 的 report_seq=1 不应冲突: %v", err)
	}
}

// TestRecordV2AtomicReportDrivesCallState 补报必须驱动 call 账本，并把 phase/final/data 记住。
func TestRecordV2AtomicReportDrivesCallState(t *testing.T) {
	cases := []struct {
		phase       string
		final       bool
		wantObserve string
		wantExec    string
		wantPhase   string
	}{
		{"accepted", false, CallObserveWaiting, CallExecRunning, ""},
		{"approved", false, CallObserveWaiting, CallExecRunning, ""},
		{"done", true, CallObserveClosed, CallExecDone, CallPhaseDone},
		{"failed", true, CallObserveClosed, CallExecFailed, CallPhaseFailed},
		{"timeout", true, CallObserveClosed, CallExecFailed, CallPhaseTimeout},
		{"cancelled", true, CallObserveClosed, CallExecCancelled, CallPhaseCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.phase, func(t *testing.T) {
			s := openTemp(t)
			ctx := context.Background()
			ensureCallsWork(t, s, "w1", "alice")
			callID := createV2Call(t, s, "w1", "shell")

			res, err := s.RecordV2Atomic(ctx, recReport("k1", recHashA, "shell", "r1", callID, tc.phase, 1, tc.final))
			if err != nil {
				t.Fatal(err)
			}
			observe, exec, phase, resultRef := callState(t, s, callID)
			if observe != tc.wantObserve || exec != tc.wantExec || phase != tc.wantPhase {
				t.Fatalf("补报 phase=%s 后账本应为 (%s,%s,%q)，实际 (%s,%s,%q)",
					tc.phase, tc.wantObserve, tc.wantExec, tc.wantPhase, observe, exec, phase)
			}
			if tc.final && resultRef != res.ItemID {
				t.Fatalf("终态报告的 result_ref 应指向刚落库的消息，实际 %q vs %q", resultRef, res.ItemID)
			}
			if !tc.final && resultRef != "" {
				t.Fatalf("非终态报告不该有 result_ref，实际 %q", resultRef)
			}
			// 回执必须记住补报说了什么，不能只留一个 hash。
			var (
				phase2  string
				final   int
				data    []byte
				itemID  string
				callID2 string
				seq     int64
			)
			err = s.db.QueryRowContext(ctx,
				`SELECT phase, final, data, item_id, call_id, report_seq FROM report_receipts
				 WHERE work='w1' AND source_service='shell' AND report_id='r1'`,
			).Scan(&phase2, &final, &data, &itemID, &callID2, &seq)
			if err != nil {
				t.Fatalf("补报回执读不回: %v", err)
			}
			if phase2 != tc.phase || final != btoi(tc.final) || itemID != res.ItemID ||
				callID2 != callID || seq != 1 || len(data) == 0 {
				t.Fatalf("补报回执字段未持久化：phase=%q final=%d item=%q call=%q seq=%d data=%d 字节",
					phase2, final, itemID, callID2, seq, len(data))
			}
		})
	}
}

// callRemote 读一行 call 已回填的 owner 受理句柄。
func callRemote(t *testing.T, st *Store, callID string) string {
	t.Helper()
	var remote string
	if err := st.db.QueryRow(`SELECT COALESCE(remote_item_id, '') FROM calls WHERE id = ?`, callID).Scan(&remote); err != nil {
		t.Fatalf("读取 call %s 的 remote_item_id: %v", callID, err)
	}
	return remote
}

// TestRecordV2AtomicRemoteItemWrittenOnce owner 句柄只回填一次（R-030）：
// 首次非空值写入；同值幂等；不同值保留原值并回 RemoteMismatch=true；空值忽略。
func TestRecordV2AtomicRemoteItemWrittenOnce(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	ensureCallsWork(t, s, "w1", "alice")
	callID := createV2Call(t, s, "w1", "shell")

	first := recReport("k1", recHashA, "shell", "r1", callID, "accepted", 1, false)
	first.Report.RemoteItemID = "owner-item-1"
	res, err := s.RecordV2Atomic(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteMismatch {
		t.Fatal("首次回填不应标记冲突")
	}
	if got := callRemote(t, s, callID); got != "owner-item-1" {
		t.Fatalf("首次非空句柄应回填，实际 %q", got)
	}

	same := recReport("k2", recHashB, "shell", "r2", callID, "approved", 2, false)
	same.Report.RemoteItemID = "owner-item-1"
	res, err = s.RecordV2Atomic(ctx, same)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteMismatch {
		t.Fatal("同值句柄不应标记冲突")
	}

	diff := recReport("k3", recHashA, "shell", "r3", callID, "done", 3, true)
	diff.Report.RemoteItemID = "owner-item-2"
	res, err = s.RecordV2Atomic(ctx, diff)
	if err != nil {
		t.Fatal(err)
	}
	if !res.RemoteMismatch {
		t.Fatal("不同句柄应回 RemoteMismatch=true（可见，但不回滚补报）")
	}
	if got := callRemote(t, s, callID); got != "owner-item-1" {
		t.Fatalf("冲突时不得覆盖首个句柄，实际 %q", got)
	}

	// 空值忽略：另开一条 call，避免在已关闭的账本上制造非法转移。
	other := createV2Call(t, s, "w1", "shell")
	empty := recReport("k4", recHashB, "shell", "r4", other, "accepted", 1, false)
	empty.Report.RemoteItemID = ""
	res, err = s.RecordV2Atomic(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	if res.RemoteMismatch || callRemote(t, s, other) != "" {
		t.Fatalf("空句柄应忽略（mismatch=%v remote=%q）", res.RemoteMismatch, callRemote(t, s, other))
	}
}

// TestRecordV2AtomicRemoteItemUniqueCollision 句柄已被同一 (service, work) 的另一条 call
// 占用（部分唯一索引）：不换绑、不失败，回 RemoteMismatch=true，补报照常入账。
func TestRecordV2AtomicRemoteItemUniqueCollision(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	ensureCallsWork(t, s, "w1", "alice")
	holder := createV2Call(t, s, "w1", "shell")
	if err := s.SetCallRemote(ctx, holder, "taken-handle"); err != nil {
		t.Fatal(err)
	}
	callID := createV2Call(t, s, "w1", "shell")
	res, err := s.RecordV2Atomic(ctx, func() V2RecordInput {
		in := recReport("k1", recHashA, "shell", "r1", callID, "accepted", 1, false)
		in.Report.RemoteItemID = "taken-handle"
		return in
	}())
	if err != nil {
		t.Fatalf("句柄撞键不应让补报失败: %v", err)
	}
	if !res.RemoteMismatch {
		t.Fatal("句柄撞键应回 RemoteMismatch=true")
	}
	if got := callRemote(t, s, callID); got != "" {
		t.Fatalf("撞键的 call 不应获得别人的句柄，实际 %q", got)
	}
	if observe, exec, _, _ := callState(t, s, callID); observe != CallObserveWaiting || exec != CallExecRunning {
		t.Fatalf("补报仍应按 phase 推进，实际 (%s,%s)", observe, exec)
	}
}

// TestRecordV2AtomicReportRejectsMismatchedLedger 补报与账本对不上时整笔拒绝，一条消息都不留。
func TestRecordV2AtomicReportRejectsMismatchedLedger(t *testing.T) {
	cases := []struct {
		name   string
		source string
		work   string
		callID string
		want   error
	}{
		{"call 不存在", "shell", "w1", "nope", ErrCallNotFound},
		{"owner 服务不符", "timer", "w1", "", ErrCallConflict},
		{"work 不符", "shell", "w2", "", ErrCallConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := openTemp(t)
			ctx := context.Background()
			ensureCallsWork(t, s, "w1", "alice")
			if tc.work == "w2" {
				ensureCallsWork(t, s, "w2", "bob")
			}
			callID := tc.callID
			if callID == "" {
				callID = createV2Call(t, s, "w1", "shell")
			}
			in := recReport("k1", recHashA, tc.source, "r1", callID, "done", 1, true)
			in.WorkID = tc.work
			if _, err := s.RecordV2Atomic(ctx, in); !errors.Is(err, tc.want) {
				t.Fatalf("应回 %v，实际 %v", tc.want, err)
			}
			if n := countTable(t, s, `SELECT COUNT(*) FROM messages WHERE work_id = ?`, tc.work); n != 0 {
				t.Fatalf("被拒绝的补报不应留下消息，实际 %d 行", n)
			}
			if n := countTable(t, s, `SELECT COUNT(*) FROM report_receipts WHERE work = ?`, tc.work); n != 0 {
				t.Fatalf("被拒绝的补报不应留下回执，实际 %d 行", n)
			}
		})
	}
}

// TestRecordV2AtomicReportCannotReviveCancelledCall 已确认取消的调用不能被补报改成完成。
func TestRecordV2AtomicReportCannotReviveCancelledCall(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	ensureCallsWork(t, s, "w1", "alice")
	callID := createV2Call(t, s, "w1", "shell")
	if _, err := s.db.Exec(`UPDATE calls SET observe_state = ?, exec_state = ?, phase = ?, reason = ? WHERE id = ?`,
		CallObserveClosed, CallExecCancelled, CallPhaseCancelled, "cancelled", callID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RecordV2Atomic(ctx, recReport("k1", recHashA, "shell", "r1", callID, "done", 1, true)); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("cancelled → done 是非法转移，应回 ErrCallConflict，实际 %v", err)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("非法补报不应留下消息，实际 %d 行", n)
	}
}

// TestRecordV2AtomicRequiresPayloadHash 走幂等路径必须有 hash：没有 hash 就无从判重放。
func TestRecordV2AtomicRequiresPayloadHash(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	in := recInput("k1", "")
	if _, err := s.RecordV2Atomic(ctx, in); err == nil {
		t.Fatal("带 client_item_id 却缺 payload_hash 应报错")
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 0 {
		t.Fatalf("参数不合法不应留下任何行，实际 %d 行", n)
	}
}

// TestRecordV2AtomicAllocatesSeqFromCounters 序号只由 seq_counters 决定：
// 库里已有高序号（老路径显式给的 seq=50）时，取号必须不早于它。
func TestRecordV2AtomicAllocatesSeqFromCounters(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.EnsureConversation("c1", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.Record("c1", "user", "老路径的话", "user", "", 50, ""); err != nil {
		t.Fatal(err)
	}
	res, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq != 51 {
		t.Fatalf("取号应不早于已落库的最大 seq，期望 51，实际 %d", res.Seq)
	}
	var counter int64
	if err := s.db.QueryRow(`SELECT last_seq FROM seq_counters WHERE conversation_id='c1'`).Scan(&counter); err != nil {
		t.Fatal(err)
	}
	if counter != 51 {
		t.Fatalf("seq_counters 应与实际落库序号一致，期望 51，实际 %d", counter)
	}
}

// TestRecordV2AtomicSeqBeatsStaleCounter 计数器落后于表内事实时必须以表为准。
//
// 场景是「计数器已存在，但老路径又显式写了更高的 seq」：播种语句此时是 INSERT OR IGNORE，
// 什么都不会做；如果取号只看 last_seq + 1，就会发出一个已经用过的序号（历史里出现两个
// seq=1 的行，按 seq 排序的上下文从此不可信）。这条用例专杀「去掉 messages 上界」的写法。
func TestRecordV2AtomicSeqBeatsStaleCounter(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	if err := s.EnsureConversation("c1", "", ""); err != nil {
		t.Fatal(err)
	}
	// 1) 先正常落一条：计数器被建起来，last_seq = 1。
	if _, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA)); err != nil {
		t.Fatal(err)
	}
	// 2) 老路径显式写高号（不推进计数器）。
	if err := s.Record("c1", "user", "老路径的高号", "user", "", 50, ""); err != nil {
		t.Fatal(err)
	}
	// 3) 再取号必须跳过 2..50（正文换掉，避免撞上消息层的连续相同去重）。
	third := recInput("k2", recHashB)
	third.Content = "第三条"
	res, err := s.RecordV2Atomic(ctx, third)
	if err != nil {
		t.Fatal(err)
	}
	if res.Seq != 51 {
		t.Fatalf("计数器落后时取号应跟上表内事实，期望 51，实际 %d", res.Seq)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages WHERE seq = 51`); n != 1 {
		t.Fatalf("seq=51 应恰好一行，实际 %d", n)
	}
	// 同会话内 seq 不得重复（重号 = 按 seq 排序的上下文不可信）。
	if n := countTable(t, s, `SELECT COUNT(*) FROM (SELECT seq FROM messages WHERE conversation_id='c1' GROUP BY seq HAVING COUNT(*) > 1)`); n != 0 {
		t.Fatalf("会话 c1 内有 %d 个重复序号", n)
	}
}

// TestRecordV2AtomicDefaultsVisibility 未带 visibility 的 v2 消息按 "work" 落库，
// 与老路径 recordVia 的归一一致（INV-14：跨 work 默认不可见）。
func TestRecordV2AtomicDefaultsVisibility(t *testing.T) {
	s := openTemp(t)
	in := recInput("k1", recHashA)
	in.Visibility = ""
	res, err := s.RecordV2Atomic(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	if err := s.db.QueryRow(`SELECT visibility FROM messages WHERE id = ?`, res.ItemID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got != "work" {
		t.Fatalf("未标注 visibility 应落为 \"work\"，实际 %q", got)
	}
}

// TestRecordV2AtomicKeepsLegacyDialogueDedup 旧的「连续相同对话只留一行」语义保留，
// 但两个不同的前端键各自都有回执，且都指向被保留的那一行。
func TestRecordV2AtomicKeepsLegacyDialogueDedup(t *testing.T) {
	s := openTemp(t)
	ctx := context.Background()
	// 两条请求内容相同但 hash 不同（例如 meta 不一样）：消息层去重命中，回执各记各的。
	first, err := s.RecordV2Atomic(ctx, recInput("k1", recHashA))
	if err != nil {
		t.Fatal(err)
	}
	second := recInput("k2", recHashB)
	if _, err := s.RecordV2Atomic(ctx, second); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM messages`); n != 1 {
		t.Fatalf("连续相同的对话消息应仍只留一行，实际 %d 行", n)
	}
	if n := countTable(t, s, `SELECT COUNT(*) FROM store_receipts WHERE item_id = ?`, first.ItemID); n != 2 {
		t.Fatalf("两份回执都应指向被保留的那一行，实际 %d 行", n)
	}
}
