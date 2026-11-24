package store

// v2 落库的唯一原子入口（S-001 §3.0 / §4.3 / INV-13）。
//
// 一条 store 请求可能同时牵动四处事实：消息行、store 幂等回执、补报回执、call 账本。
// 它们必须在**同一个 SQLite 事务**里落地——任何一步失败就整体回滚，调用方的 ACK 只在
// 提交之后返回。分开写会留下「消息进了、回执没进」的窗口：重放时同一条补报再落一条
// 消息，回执却指向旧消息，幂等承诺当场失效（冲突载荷同理：先写消息再判冲突，等于
// 用一个错误 ACK 换一行脏数据）。
//
// 判定顺序（规则来自 S-001 §3.0 规则 3 / 规则 6）：
//  1. (work, client_item_id) 命中且同 hash、且已带 item_id → 重放，回既有回执；
//  2. (work, source_service, report_id) 命中且同 hash → 重放，回既有回执
//     ——**换 client_item_id 也算重放**：补报的身份是 report_id，不是前端键；
//  3. 同 (work, source_service, call_id, report_seq) 被别的 report_id 占了 → conflict，
//     且必须在写任何消息之前判出来；
//  4. 载荷不同（同键不同 hash）→ conflict，不写任何东西；
//  5. 以上都不命中 → 取号、写消息、写回执、按补报推进 call 账本，一次提交。
//
// seq 只由 seq_counters 在本事务内分配（S-001 §4.7）：调用方传来的序号不参与落库，
// 否则两个写入方可以约定同一个号。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrPayloadConflict 表示同一个幂等键已经用**不同**的载荷落过库。
//
// 这不是可重试的竞态，而是调用方两次说了不一样的话（S-001 §3.0 规则 3、§6.2）：
// 同 client_item_id 不同载荷 / 同 report_id 不同载荷 / 同 (call_id, report_seq) 换了
// report_id 都归到这里，交人工处置。
var ErrPayloadConflict = errors.New("幂等键已存在但载荷不同")

// V2ReportInput 是一次补报的入账元数据（protocol.StoreReport 的持久化形态）。
//
// Phase / Final / Data 都要落库：它们是 call 账本的唯一驱动依据，只在 hash 里出现
// 等于没记住——对账和展示都读不回来。Payload 的 hash 由调用方算好传入（V2RecordInput）。
type V2ReportInput struct {
	SourceService string // 发起补报的 owner 服务名，必须等于 call 的 service
	ReportID      string // 补报幂等键
	ReportSeq     int64  // 同一次调用的补报序号，从 1 开始
	CallID        string // call_open 分配的 call_id
	Phase         string // accepted / approved / done / failed / timeout / cancelled
	Final         bool   // 终态报告才为 true
	Data          []byte // owner 自定的结构化补充，原样透传
}

// V2RecordInput 是一次 v2 store 请求的全部事实。
type V2RecordInput struct {
	ConversationID string
	Role           string
	Kind           string
	Content        string
	Meta           string
	TaskID         string
	WorkID         string
	Visibility     string
	ClientItemID   string // 空 = 不走前端幂等键（只走补报或纯追加）
	PayloadHash    string // sha256(JCS(载荷))，store 与 report 回执共用
	Report         *V2ReportInput
}

// V2RecordResult 是落库回执。Deduped=true 表示本次没有新行，ItemID / Seq 是既有那条。
type V2RecordResult struct {
	ItemID  string
	Seq     int64
	Deduped bool
}

// RecordV2Atomic 在一个事务里完成「幂等判定 + 落消息 + 落回执 + 推进 call」。
//
// 错误语义（调用方据此翻错误码）：
//   - ErrPayloadConflict：幂等键冲突（同键不同载荷，或换 report_id 抢占 report_seq）；
//   - ErrCallNotFound：补报引用了本地账本里没有的 call；
//   - ErrCallConflict：补报与 call 账本对不上（work / service 不符，或状态机不允许该转移）；
//   - 其余：数据库错误。
//
// 任何错误都不留下部分写入。
func (s *Store) RecordV2Atomic(ctx context.Context, in V2RecordInput) (V2RecordResult, error) {
	work := strings.TrimSpace(in.WorkID)
	conv := strings.TrimSpace(in.ConversationID)
	if work == "" {
		return V2RecordResult{}, errors.New("RecordV2Atomic: work 必填")
	}
	if conv == "" {
		return V2RecordResult{}, errors.New("RecordV2Atomic: conversation_id 必填")
	}
	if in.ClientItemID != "" || in.Report != nil {
		if strings.TrimSpace(in.PayloadHash) == "" {
			return V2RecordResult{}, errors.New("RecordV2Atomic: 走幂等路径时 payload_hash 必填")
		}
	}
	if in.Report != nil {
		if err := checkReportShape(in.Report); err != nil {
			return V2RecordResult{}, err
		}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return V2RecordResult{}, fmt.Errorf("开始 v2 落库事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// 1) 前端幂等键命中：既有回执就是答案（同 hash），或者 conflict（不同 hash）。
	//    item_id 为空是旧两阶段路径崩在中间留下的占位：本次写完后回填它，不再插第二行。
	var fillPlaceholder bool
	if in.ClientItemID != "" {
		var storedHash, storedItem string
		err := tx.QueryRowContext(ctx,
			`SELECT payload_hash, item_id FROM store_receipts WHERE work = ? AND client_item_id = ?`,
			work, in.ClientItemID).Scan(&storedHash, &storedItem)
		switch {
		case err == nil:
			if storedHash != in.PayloadHash {
				return V2RecordResult{}, ErrPayloadConflict
			}
			if storedItem != "" {
				seq, err := seqOfItem(ctx, tx, storedItem)
				if err != nil {
					return V2RecordResult{}, err
				}
				if err := tx.Commit(); err != nil {
					return V2RecordResult{}, fmt.Errorf("提交 store 重放回执: %w", err)
				}
				return V2RecordResult{ItemID: storedItem, Seq: seq, Deduped: true}, nil
			}
			fillPlaceholder = true
		case !errors.Is(err, sql.ErrNoRows):
			return V2RecordResult{}, fmt.Errorf("查询 store 幂等记录: %w", err)
		}
	}

	// 2) 补报键命中：换 client_item_id 也是同一次补报，直接回既有回执，绝不重复入账。
	//    report_seq 被别的 report_id 占用要在写消息之前判出来（规则 3）。
	if in.Report != nil {
		var storedHash, storedItem string
		err := tx.QueryRowContext(ctx,
			`SELECT payload_hash, item_id FROM report_receipts
			 WHERE work = ? AND source_service = ? AND report_id = ?`,
			work, in.Report.SourceService, in.Report.ReportID).Scan(&storedHash, &storedItem)
		switch {
		case err == nil:
			if storedHash != in.PayloadHash {
				return V2RecordResult{}, ErrPayloadConflict
			}
			seq, err := seqOfItem(ctx, tx, storedItem)
			if err != nil {
				return V2RecordResult{}, err
			}
			if err := tx.Commit(); err != nil {
				return V2RecordResult{}, fmt.Errorf("提交补报重放回执: %w", err)
			}
			return V2RecordResult{ItemID: storedItem, Seq: seq, Deduped: true}, nil
		case !errors.Is(err, sql.ErrNoRows):
			return V2RecordResult{}, fmt.Errorf("查询补报幂等记录: %w", err)
		}
		var other string
		err = tx.QueryRowContext(ctx,
			`SELECT report_id FROM report_receipts
			 WHERE work = ? AND source_service = ? AND call_id = ? AND report_seq = ?`,
			work, in.Report.SourceService, in.Report.CallID, in.Report.ReportSeq).Scan(&other)
		if err == nil {
			// 查到了却是另一个 report_id：换 ID 重放同一个 (call_id, report_seq)。
			return V2RecordResult{}, ErrPayloadConflict
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return V2RecordResult{}, fmt.Errorf("检查补报 report_seq 占用: %w", err)
		}
	}

	// 3) 落消息（会话内连续相同的 user/assistant 仍按老规则去重，回执指向保留那条；
	//    去重命中不取号，否则白烧一个序号）。
	itemID, seqOut, dedupedMsg, err := insertMessageV2(ctx, tx, in)
	if err != nil {
		return V2RecordResult{}, err
	}
	// 真正新写一条时才推会话活跃时间；去重命中不推（没有新事实）。
	if !dedupedMsg {
		if err := touchConversationVia(ctx, tx, conv, time.Now()); err != nil {
			return V2RecordResult{}, fmt.Errorf("刷新会话 %s 的活跃时间: %w", conv, err)
		}
	}

	// 4) 落回执（与消息同一事务，所以不会再出现「有回执没消息」或反之）。
	if in.ClientItemID != "" {
		if fillPlaceholder {
			if _, err := tx.ExecContext(ctx,
				`UPDATE store_receipts SET item_id = ? WHERE work = ? AND client_item_id = ? AND item_id = ''`,
				itemID, work, in.ClientItemID); err != nil {
				return V2RecordResult{}, fmt.Errorf("回填 store 幂等记录的 item_id: %w", err)
			}
		} else {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO store_receipts(work, client_item_id, payload_hash, item_id, created_at) VALUES(?,?,?,?,?)`,
				work, in.ClientItemID, in.PayloadHash, itemID, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				return V2RecordResult{}, fmt.Errorf("写入 store 幂等记录: %w", mapReceiptConflict(err))
			}
		}
	}
	if in.Report != nil {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO report_receipts(work, source_service, report_id, payload_hash, item_id,
			   call_id, report_seq, phase, final, data, created_at)
			 VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			work, in.Report.SourceService, in.Report.ReportID, in.PayloadHash, itemID,
			in.Report.CallID, in.Report.ReportSeq, in.Report.Phase, btoi(in.Report.Final),
			reportDataArg(in.Report.Data), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
			return V2RecordResult{}, fmt.Errorf("写入补报幂等记录: %w", mapReceiptConflict(err))
		}
		if err := applyReportToCall(ctx, tx, work, in.Report, itemID); err != nil {
			return V2RecordResult{}, err
		}
	}

	if err := tx.Commit(); err != nil {
		return V2RecordResult{}, fmt.Errorf("提交 v2 落库: %w", err)
	}
	return V2RecordResult{ItemID: itemID, Seq: seqOut, Deduped: dedupedMsg}, nil
}

// checkReportShape 校验补报的最小自洽性（词汇表见 S-001 §3.9）。
func checkReportShape(r *V2ReportInput) error {
	if strings.TrimSpace(r.SourceService) == "" || strings.TrimSpace(r.ReportID) == "" || strings.TrimSpace(r.CallID) == "" {
		return errors.New("RecordV2Atomic: 补报 source_service / report_id / call_id 必填")
	}
	if r.ReportSeq <= 0 {
		return fmt.Errorf("RecordV2Atomic: 补报 report_seq 必须从 1 开始，收到 %d", r.ReportSeq)
	}
	switch r.Phase {
	case "accepted", "approved":
		if r.Final {
			return fmt.Errorf("RecordV2Atomic: 补报 phase=%s 不能同时 final=true", r.Phase)
		}
	case "done", "failed", "timeout", "cancelled":
		if !r.Final {
			return fmt.Errorf("RecordV2Atomic: 补报 phase=%s 必须 final=true", r.Phase)
		}
	default:
		return fmt.Errorf("RecordV2Atomic: 补报 phase %q 非法（accepted|approved|done|failed|timeout|cancelled）", r.Phase)
	}
	return nil
}

// seqOfItem 取既有回执指向消息的会话内 seq（重放路径必须回原值，不能回本次请求的号）。
func seqOfItem(ctx context.Context, tx *sql.Tx, itemID string) (int64, error) {
	id, err := strconv.ParseInt(itemID, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("RecordV2Atomic: 回执里的 item_id %q 不是消息 id: %w", itemID, err)
	}
	var seq int64
	err = tx.QueryRowContext(ctx, `SELECT seq FROM messages WHERE id = ?`, id).Scan(&seq)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("%w: 幂等回执指向的消息 %s 不存在", ErrPayloadConflict, itemID)
	}
	if err != nil {
		return 0, fmt.Errorf("读取既有回执的消息 seq: %w", err)
	}
	return seq, nil
}

// insertMessageV2 在事务里落一条 v2 消息并返回回执。
//
// 与老 Record 路径的差别只有两点：显式写 work_id / visibility，且 seq **恒由
// seq_counters 分配**（调用方传来的序号不参与，§4.7）。「连续完全相同的
// user/assistant 跳过」仍按既有语义生效，去重命中时不取号、回被保留那一条的 id / seq。
func insertMessageV2(ctx context.Context, tx *sql.Tx, in V2RecordInput) (string, int64, bool, error) {
	kind := in.Kind
	if kind == "" {
		kind = kindOfRole(in.Role)
	}
	if kind == "user" || kind == "assistant" {
		var (
			lastID  int64
			lastSeq int64
			r, c, k string
		)
		err := tx.QueryRowContext(ctx,
			`SELECT id, seq, role, content, kind FROM messages
			 WHERE conversation_id=? AND kind IN ('', 'user', 'assistant')
			 ORDER BY seq DESC, id DESC LIMIT 1`,
			in.ConversationID,
		).Scan(&lastID, &lastSeq, &r, &c, &k)
		if err == nil && r == in.Role && c == in.Content && (k == kind || k == "") {
			return strconv.FormatInt(lastID, 10), lastSeq, true, nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return "", 0, false, err
		}
	}
	seq, err := allocSeqVia(ctx, tx, in.ConversationID)
	if err != nil {
		return "", 0, false, err
	}
	// 未标注的 visibility 归一为 "work"：与老路径 recordVia 的默认一致（INV-14）。
	visibility := in.Visibility
	if visibility == "" {
		visibility = "work"
	}
	var id int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO messages(conversation_id, role, content, kind, meta, seq, created_at, task_id, work_id, visibility)
		 VALUES(?,?,?,?,?,?,?,?,?,?) RETURNING id`,
		in.ConversationID, in.Role, in.Content, kind, in.Meta, seq,
		time.Now().Format(time.RFC3339Nano), in.TaskID, in.WorkID, visibility,
	).Scan(&id)
	if err != nil {
		return "", 0, false, err
	}
	return strconv.FormatInt(id, 10), seq, false, nil
}

// loadCallForReport 读一行 call 并返回它的归属与两维状态（补报入账前核对用）。
func loadCallForReport(ctx context.Context, tx *sql.Tx, callID string) (callWork, service, observe, exec string, err error) {
	err = tx.QueryRowContext(ctx,
		`SELECT work, service, observe_state, exec_state FROM calls WHERE id = ?`,
		callID).Scan(&callWork, &service, &observe, &exec)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", "", "", fmt.Errorf("%w: %s", ErrCallNotFound, callID)
	}
	if err != nil {
		return "", "", "", "", fmt.Errorf("读取 call %s: %w", callID, err)
	}
	return callWork, service, observe, exec, nil
}

// applyReportToCall 按补报的 phase 推进 call 账本（S-001 §5.1 两维状态机）。
//
//   - accepted / approved：owner 已受理 / 审批通过，只是「结果还没来」——
//     推进 exec=running、observe 回 waiting，**不写终态 phase**；
//   - done / failed / timeout / cancelled：终态报告，exec 落对应值、phase 落报告值、
//     observe=closed，result_ref 指向刚落库的消息（结果先落库、同一事务里关闭）。
//
// 非法转移一律 ErrCallConflict，整笔回滚：补报不能让已取消的调用「复活成完成」。
func applyReportToCall(ctx context.Context, tx *sql.Tx, work string, r *V2ReportInput, itemID string) error {
	callWork, service, observe, exec, err := loadCallForReport(ctx, tx, r.CallID)
	if err != nil {
		return err
	}
	if strings.TrimSpace(callWork) != work {
		return fmt.Errorf("%w: call %s 属于 work %s，补报却按 work %s 入账",
			ErrCallConflict, r.CallID, callWork, work)
	}
	if strings.TrimSpace(service) != strings.TrimSpace(r.SourceService) {
		return fmt.Errorf("%w: call %s 的 owner 是 %s，补报却来自 %s",
			ErrCallConflict, r.CallID, service, r.SourceService)
	}

	toObserve, toExec := observe, exec
	toPhase := "" // 空 = 保持原值；终态报告才写 phase，也才把 result_ref 指向本次消息
	if exec == CallExecCreated {
		// created 是「本地意图、派发未确认」。owner 自己的补报（来源服务已核对一致）
		// 就是「已受理」的证据，按 S-001 §3.0.2 对账表第二行「owner 回答已受理 →
		// 按真实状态更新」先把它推进到 running，再走结果入账的常规转移。
		// 状态机本身没有放宽：SetCallState 仍然拒绝 created → 终态，
		// 少这一跳的话，任何「owner 第一次报告就是终态」的补报都会被误判成非法。
		if !canExec(CallExecCreated, CallExecRunning) {
			return fmt.Errorf("%w: call %s exec %s → %s", ErrCallConflict, r.CallID, exec, CallExecRunning)
		}
		exec = CallExecRunning
	}
	switch r.Phase {
	case "accepted", "approved":
		toObserve, toExec = CallObserveWaiting, CallExecRunning
	case "done":
		toObserve, toExec, toPhase = CallObserveClosed, CallExecDone, CallPhaseDone
	case "failed":
		toObserve, toExec, toPhase = CallObserveClosed, CallExecFailed, CallPhaseFailed
	case "timeout":
		toObserve, toExec, toPhase = CallObserveClosed, CallExecFailed, CallPhaseTimeout
	case "cancelled":
		toObserve, toExec, toPhase = CallObserveClosed, CallExecCancelled, CallPhaseCancelled
	default:
		return fmt.Errorf("%w: 补报 phase %q 不驱动 call 账本", ErrCallConflict, r.Phase)
	}
	if !canObserve(observe, toObserve) {
		return fmt.Errorf("%w: call %s observe %s → %s", ErrCallConflict, r.CallID, observe, toObserve)
	}
	if !canExec(exec, toExec) {
		return fmt.Errorf("%w: call %s exec %s → %s", ErrCallConflict, r.CallID, exec, toExec)
	}

	now := callTimestamp()
	if toPhase == "" {
		// 非终态报告：phase / result_ref 保持原值（没有结果可指）。
		if _, err := tx.ExecContext(ctx,
			`UPDATE calls SET observe_state = ?, exec_state = ?, updated_at = ? WHERE id = ?`,
			toObserve, toExec, now, r.CallID); err != nil {
			return fmt.Errorf("推进 call %s（%s）: %w", r.CallID, r.Phase, err)
		}
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE calls SET observe_state = ?, exec_state = ?, phase = ?, reason = '', result_ref = ?, updated_at = ?
		 WHERE id = ?`,
		toObserve, toExec, toPhase, itemID, now, r.CallID); err != nil {
		return fmt.Errorf("关闭 call %s（%s）: %w", r.CallID, r.Phase, err)
	}
	return nil
}

// reportDataArg 把空的补报 data 落成 NULL（区分「没有补充」与「补充是空 msgpack」）。
func reportDataArg(data []byte) any {
	if len(data) == 0 {
		return nil
	}
	return data
}

// mapReceiptConflict 把回执表上的唯一约束错误翻成幂等冲突：
// 走到这里说明同一事务内的判定与写入之间被别的写入方插队（SQLite 单写者下几乎不可能，
// 但约束是真的，不能当 internal 糊过去）。
func mapReceiptConflict(err error) error {
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return ErrPayloadConflict
	}
	return err
}
