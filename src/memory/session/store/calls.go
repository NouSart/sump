package store

// 调用账本（S-001 §3.0.2 / §3.4 / §4.5 / §5.1）：call 行、两维状态机、control 幂等记录
// 与常驻目录。这里只做 memory 的持久化与状态校验，不承担执行语义：
//
//   - `created` 是本地意图（call_open 之后、owner 受理之前），不是执行超时；
//   - pause / resume 必须等 owner 确认（会话层注入的 owner 客户端），因此 store **不提供**
//     「直接把状态写成 paused」的快捷入口——那等于允许调用方本地假装控制成功；
//   - calls 表没有 task / owner / instance 列：task 与 owner 从 works 行联表解析
//     （GetCall / ListResident 都带上）；instance 在当前 schema 与协议里都没有落点，不预留列。
//
// 两维状态机（S-001 §5.1）：
//
//	observe: waiting / not_waiting / closed
//	exec:    created / running / paused / done / failed / cancelled / unknown
//
// 状态转移只有 canObserve / canExec 一个口径：补报（record_v2.go 的 applyReportToCall）
// 与本文件的 SetCallState 共用它们，不复制第二套判定。非法转移一律返回 ErrCallConflict
// （哨兵，调用方用 errors.Is 判定）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/spv2"
)

const (
	CallObserveWaiting    = "waiting"
	CallObserveNotWaiting = "not_waiting"
	CallObserveClosed     = "closed"

	CallExecCreated   = "created"
	CallExecRunning   = "running"
	CallExecPaused    = "paused"
	CallExecDone      = "done"
	CallExecFailed    = "failed"
	CallExecCancelled = "cancelled"
	CallExecUnknown   = "unknown"

	CallPhaseDone      = "done"
	CallPhaseFailed    = "failed"
	CallPhaseTimeout   = "timeout"
	CallPhaseCancelled = "cancelled"
	CallPhaseOrphaned  = "orphaned"
	CallPhaseUnknown   = "unknown"

	CallModeOnce     = "once"
	CallModeResident = "resident"
)

// 调用账本的哨兵错误。非法状态转移一律用 ErrCallConflict 包裹（%w）。
var (
	ErrCallConflict    = errors.New("call 状态冲突")
	ErrCallNotFound    = errors.New("call 不存在")
	ErrControlNotFound = errors.New("call 控制记录不存在")
)

// CallRow 是 calls 表的一行。
//
// Owner / Task 来自 works 的联表（calls 表没有这两列）；它们不是 calls 自己的字段，
// 因此只读，写入永远经由 works。
type CallRow struct {
	ID       string
	Work     string
	Task     string // works.task（work 关联的任务；可为空）
	Owner    string // works.owner_id（常驻目录的隔离键）
	Service  string
	Action   string
	Mode     string
	Pausable bool

	TTLMS          int64
	ExecDeadlineMS int64

	RemoteItemID      string // 受理后回填；受理前为空
	Observe           string
	Exec              string
	Phase             string // 空 = NULL（不写 phase）
	Reason            string
	ResultRef         string
	RemoteMayContinue bool

	CreatedAt string
	UpdatedAt string
}

// ControlRow 是 call_controls 表的一行（control_id 幂等记录）。
type ControlRow struct {
	ControlID string
	CallID    string
	Work      string
	Op        string
	TTLMS     int64
	Observe   string
	Exec      string
	Reason    string
	Result    string // 首次回执的 JSON（observe/exec/reason），空 = 尚未落库
	CreatedAt string
	UpdatedAt string
}

// ControlResult 是控制操作的首次回执（持久化在 call_controls 的 observe/exec/reason）。
// PutControl 返回的 result 字符串就是这个结构的 JSON：同 control_id 重放时原样返回首次结果。
type ControlResult struct {
	Observe string `json:"observe"`
	Exec    string `json:"exec"`
	Reason  string `json:"reason"`
}

// callTimestamp 固定 9 位小数（纳秒），保证字符串排序 = 时间排序。
func callTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---------- 状态机 ----------

func validObserve(s string) bool {
	switch s {
	case CallObserveWaiting, CallObserveNotWaiting, CallObserveClosed:
		return true
	}
	return false
}

func validExec(s string) bool {
	switch s {
	case CallExecCreated, CallExecRunning, CallExecPaused, CallExecDone,
		CallExecFailed, CallExecCancelled, CallExecUnknown:
		return true
	}
	return false
}

func validPhase(s string) bool {
	switch s {
	case "", CallPhaseDone, CallPhaseFailed, CallPhaseTimeout,
		CallPhaseCancelled, CallPhaseOrphaned, CallPhaseUnknown:
		return true
	}
	return false
}

// canObserve 判断观察维转移是否合法；closed 是终态，不允许再打开。
func canObserve(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case CallObserveWaiting:
		return to == CallObserveNotWaiting || to == CallObserveClosed
	case CallObserveNotWaiting:
		// 对账拿到 owner 的确定答案后可以重新回到等待；closed 之后不行。
		return to == CallObserveWaiting || to == CallObserveClosed
	default:
		return false
	}
}

// canExec 判断执行维转移是否合法（S-001 §5.1）。
// 关键约束：created 不能直接 done（没执行过就不能有结果）；cancelled 不能再 running。
func canExec(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case CallExecCreated:
		// 本地意图：只能被派发、本地取消，或对账后变成不可判定。
		return to == CallExecRunning || to == CallExecCancelled || to == CallExecUnknown
	case CallExecRunning:
		return to == CallExecPaused || to == CallExecDone || to == CallExecFailed ||
			to == CallExecCancelled || to == CallExecUnknown
	case CallExecPaused:
		return to == CallExecRunning || to == CallExecCancelled || to == CallExecUnknown
	case CallExecUnknown:
		// 对账可以修正为任意真实状态；但不能退回 created（本地意图只属于派发前）。
		return to == CallExecRunning || to == CallExecPaused || to == CallExecDone ||
			to == CallExecFailed || to == CallExecCancelled
	default:
		// done / failed / cancelled 是终态；对账仍可能把它们修正为 unknown。
		return to == CallExecUnknown
	}
}

// ---------- 调用生命周期 ----------

// CreateCall 在派发前分配 call_id 并落一条本地意图（observe=waiting、exec=created、
// phase=NULL，S-001 §3.0.2 / §4.5）。
//
// 只接受 work / service / action / mode / pausable / 两个时限：task 与 owner 由 works
// 联表解析，不需要调用方声明，也就不存在「调用方声明与账本不一致」这个可撒谎的入口。
func (s *Store) CreateCall(ctx context.Context, work, service, action, mode string,
	pausable bool, ttlMS, execDeadlineMS int64) (string, error) {
	work = strings.TrimSpace(work)
	service = strings.TrimSpace(service)
	action = strings.TrimSpace(action)
	if work == "" {
		return "", errors.New("call_open: work 必填")
	}
	if service == "" {
		return "", errors.New("call_open: service 必填")
	}
	if ttlMS < 0 || execDeadlineMS < 0 {
		return "", fmt.Errorf("call_open: ttl_ms / exec_deadline_ms 不能为负（收到 %d / %d）", ttlMS, execDeadlineMS)
	}
	if mode == "" {
		mode = CallModeOnce
	}
	if mode != CallModeOnce && mode != CallModeResident {
		return "", fmt.Errorf("call_open: mode %q 非法（once|resident）", mode)
	}

	// calls 没有外键：不查 work 就会留下无主调用（= 第二份真相）。
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM works WHERE id = ?`, work).Scan(&n); err != nil {
		return "", fmt.Errorf("call_open: 检查 work %s: %w", work, err)
	}
	if n == 0 {
		return "", fmt.Errorf("%w: %s", ErrWorkNotFound, work)
	}

	now := callTimestamp()
	id := spv2.NewID()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO calls
		(id, work, service, action, mode, pausable, ttl_ms, exec_deadline_ms, remote_item_id,
		 observe_state, exec_state, phase, reason, result_ref, remote_may_continue, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,NULL,?,?,NULL,'','',0,?,?)`,
		id, work, service, action, mode, btoi(pausable), ttlMS, execDeadlineMS,
		CallObserveWaiting, CallExecCreated, now, now); err != nil {
		return "", fmt.Errorf("创建 call %s: %w", id, err)
	}
	return id, nil
}

// selectCallSQL 是 calls 行 + works 联表的统一投影（列顺序与 scanCallRow 对齐）。
const selectCallSQL = `SELECT c.id, c.work, c.service, c.action, c.mode, c.pausable,
	c.ttl_ms, c.exec_deadline_ms, COALESCE(c.remote_item_id, ''), c.observe_state, c.exec_state,
	COALESCE(c.phase, ''), c.reason, c.result_ref, c.remote_may_continue, c.created_at, c.updated_at,
	COALESCE(w.owner_id, ''), COALESCE(w.task, '')
	FROM calls c
	LEFT JOIN works w ON w.id = c.work`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanCallRow(r rowScanner) (CallRow, error) {
	var out CallRow
	var pausable, remoteMayContinue int
	if err := r.Scan(&out.ID, &out.Work, &out.Service, &out.Action, &out.Mode, &pausable,
		&out.TTLMS, &out.ExecDeadlineMS, &out.RemoteItemID, &out.Observe, &out.Exec,
		&out.Phase, &out.Reason, &out.ResultRef, &remoteMayContinue, &out.CreatedAt, &out.UpdatedAt,
		&out.Owner, &out.Task); err != nil {
		return CallRow{}, err
	}
	out.Pausable = pausable != 0
	out.RemoteMayContinue = remoteMayContinue != 0
	return out, nil
}

// GetCall 读一条 call（含 works 联表的 owner / task）。
func (s *Store) GetCall(ctx context.Context, id string) (CallRow, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return CallRow{}, fmt.Errorf("%w: id 必填", ErrCallNotFound)
	}
	out, err := scanCallRow(s.db.QueryRowContext(ctx, selectCallSQL+` WHERE c.id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return CallRow{}, fmt.Errorf("%w: %s", ErrCallNotFound, id)
	}
	if err != nil {
		return CallRow{}, fmt.Errorf("读取 call %s: %w", id, err)
	}
	return out, nil
}

// ---------- works.task 关联（call 账本解析 work → task 用） ----------

// LinkWorkTask 把 work 关联到一个已存在的 task（幂等；已关联别的 task 返回 ErrCallConflict）。
// 语义与 af6cfc8:src/memory/store/calls.go:629 一致：work 是归属键，一个 work 只能对上一个 task。
func (s *Store) LinkWorkTask(ctx context.Context, work, task string) error {
	work = strings.TrimSpace(work)
	task = strings.TrimSpace(task)
	if work == "" {
		return fmt.Errorf("%w: work id 必填", ErrWorkNotFound)
	}
	if task == "" {
		return fmt.Errorf("%w: task id 必填", ErrTaskNotFound)
	}
	// 先确认 work 存在：UPDATE 一行不存在会「成功改 0 行」，不能当成关联成功。
	var current string
	err := s.db.QueryRowContext(ctx, `SELECT task FROM works WHERE id = ?`, work).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrWorkNotFound, work)
	}
	if err != nil {
		return fmt.Errorf("读取 work %s 的任务: %w", work, err)
	}
	// 先确认 task 存在，再判断是否与既有归属冲突：不存在的 id 应报 ErrTaskNotFound，
	// 而不是拿「已关联别的 task」掩盖掉「这个 task 根本不存在」。
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tasks WHERE id = ?`, task).Scan(&n); err != nil {
		return fmt.Errorf("检查 task %s: %w", task, err)
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrTaskNotFound, task)
	}
	if current != "" && current != task {
		return fmt.Errorf("%w: work %s 已关联 task %s，不能再关联 %s", ErrCallConflict, work, current, task)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE works SET task = ?, updated_at = ? WHERE id = ?`,
		task, time.Now().UTC().Format(time.RFC3339), work); err != nil {
		return fmt.Errorf("关联 work %s → task %s: %w", work, task, err)
	}
	return nil
}

// SetCallState 按两维状态机更新状态；任何一维非法转移都返回 ErrCallConflict 且不落库。
// phase 为空串 = 写 NULL（S-001 §3.0.2：不可判定时「不写 phase」）。
func (s *Store) SetCallState(ctx context.Context, id, observe, exec, phase, reason string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id 必填", ErrCallNotFound)
	}
	if !validObserve(observe) {
		return fmt.Errorf("call %s: observe 取值非法 %q", id, observe)
	}
	if !validExec(exec) {
		return fmt.Errorf("call %s: exec 取值非法 %q", id, exec)
	}
	if !validPhase(phase) {
		return fmt.Errorf("call %s: phase 取值非法 %q", id, phase)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("开始 call 状态事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var curObserve, curExec string
	err = tx.QueryRowContext(ctx, `SELECT observe_state, exec_state FROM calls WHERE id = ?`, id).Scan(&curObserve, &curExec)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%w: %s", ErrCallNotFound, id)
	}
	if err != nil {
		return fmt.Errorf("读取 call %s 状态: %w", id, err)
	}
	if !canObserve(curObserve, observe) {
		return fmt.Errorf("%w: call %s observe %s → %s", ErrCallConflict, id, curObserve, observe)
	}
	if !canExec(curExec, exec) {
		return fmt.Errorf("%w: call %s exec %s → %s", ErrCallConflict, id, curExec, exec)
	}

	var phaseArg any // nil → SQL NULL
	if phase != "" {
		phaseArg = phase
	}
	if _, err := tx.ExecContext(ctx, `UPDATE calls
		SET observe_state = ?, exec_state = ?, phase = ?, reason = ?, updated_at = ?
		WHERE id = ?`, observe, exec, phaseArg, reason, callTimestamp(), id); err != nil {
		return fmt.Errorf("更新 call %s 状态: %w", id, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 call %s 状态: %w", id, err)
	}
	return nil
}

// SetCallRemoteMayContinue 标记「取消已发出，但 owner 不可暂停、远端可能还在继续」
// （S-001 §3.4：不可暂停调用的 cancel）。
func (s *Store) SetCallRemoteMayContinue(ctx context.Context, id string, mayContinue bool) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return fmt.Errorf("%w: id 必填", ErrCallNotFound)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE calls SET remote_may_continue = ?, updated_at = ? WHERE id = ?`,
		btoi(mayContinue), callTimestamp(), id)
	if err != nil {
		return fmt.Errorf("更新 call %s remote_may_continue: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrCallNotFound, id)
	}
	return nil
}

// SetCallRemote 在 owner 受理后回填 remote_item_id（受理前为 NULL）。
// 同一 service + work 下重复回填同一个 remote id 会撞部分唯一索引，按 conflict 返回。
func (s *Store) SetCallRemote(ctx context.Context, id, remoteItemID string) error {
	id = strings.TrimSpace(id)
	remoteItemID = strings.TrimSpace(remoteItemID)
	if id == "" {
		return fmt.Errorf("%w: id 必填", ErrCallNotFound)
	}
	if remoteItemID == "" {
		return errors.New("SetCallRemote: remote_item_id 必填")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE calls SET remote_item_id = ?, updated_at = ? WHERE id = ?`,
		remoteItemID, callTimestamp(), id)
	if err != nil {
		if isUniqueViolation(err) {
			return fmt.Errorf("%w: remote_item_id %q 已被同一 service/work 的其它 call 占用", ErrCallConflict, remoteItemID)
		}
		return fmt.Errorf("回填 call %s remote_item_id: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrCallNotFound, id)
	}
	return nil
}

// ---------- call_controls（control_id 幂等） ----------

func encodeControlResult(observe, exec, reason string) (string, error) {
	raw, err := json.Marshal(ControlResult{Observe: observe, Exec: exec, Reason: reason})
	if err != nil {
		return "", fmt.Errorf("编码 control 回执: %w", err)
	}
	return string(raw), nil
}

// DecodeControlResult 解析 PutControl 返回的首次回执（JSON）。
func DecodeControlResult(raw string) (ControlResult, error) {
	var out ControlResult
	if strings.TrimSpace(raw) == "" {
		return out, errors.New("control 回执为空")
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, fmt.Errorf("解析 control 回执: %w", err)
	}
	return out, nil
}

// PutControl 为 control_id 占位（幂等）。新 control 返回 ("", false, nil)，
// 调用方必须用 SetControlResult 填首次结果；同 control_id 重放返回首次结果 JSON 与 deduped=true。
//
// 已存在但结果还没落库（首次处理中断）时返回 ErrCallConflict：宁可让调用方显式重试，
// 也不返回一个假结果。call_id / op 与首次记录不一致也按 conflict 处理。
func (s *Store) PutControl(ctx context.Context, controlID, callID, op string) (string, bool, error) {
	controlID = strings.TrimSpace(controlID)
	callID = strings.TrimSpace(callID)
	op = strings.TrimSpace(op)
	if controlID == "" || callID == "" || op == "" {
		return "", false, errors.New("PutControl: control_id / call_id / op 必填")
	}
	var work string
	err := s.db.QueryRowContext(ctx, `SELECT work FROM calls WHERE id = ?`, callID).Scan(&work)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, fmt.Errorf("%w: %s", ErrCallNotFound, callID)
	}
	if err != nil {
		return "", false, fmt.Errorf("读取 call %s: %w", callID, err)
	}
	now := callTimestamp()
	res, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO call_controls
		(control_id, call_id, work, op, ttl_ms, observe, exec, reason, created_at, updated_at)
		VALUES (?,?,?,?,0,'','','',?,?)`, controlID, callID, work, op, now, now)
	if err != nil {
		return "", false, fmt.Errorf("记录 control %s: %w", controlID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return "", false, err
	}
	if n == 1 {
		return "", false, nil
	}
	row, ok, err := s.GetControl(ctx, controlID)
	if err != nil {
		return "", true, err
	}
	if !ok {
		return "", true, fmt.Errorf("control %s 写入后读不到", controlID)
	}
	if row.CallID != callID || row.Op != op {
		return "", true, fmt.Errorf("%w: control %s 已用于 call %s op %s（收到 call %s op %s）",
			ErrCallConflict, controlID, row.CallID, row.Op, callID, op)
	}
	if row.Result == "" {
		return "", true, fmt.Errorf("%w: control %s 的首次结果尚未落库", ErrCallConflict, controlID)
	}
	return row.Result, true, nil
}

// SetControlResult 落 control_id 的首次回执（observe/exec/reason）。
func (s *Store) SetControlResult(ctx context.Context, controlID, observe, exec, reason string) error {
	controlID = strings.TrimSpace(controlID)
	if controlID == "" {
		return fmt.Errorf("%w: control_id 必填", ErrControlNotFound)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE call_controls
		SET observe = ?, exec = ?, reason = ?, updated_at = ? WHERE control_id = ?`,
		observe, exec, reason, callTimestamp(), controlID)
	if err != nil {
		return fmt.Errorf("更新 control %s 回执: %w", controlID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%w: %s", ErrControlNotFound, controlID)
	}
	return nil
}

// GetControl 读一条 control 记录；ok=false 表示 control_id 尚未出现过。
func (s *Store) GetControl(ctx context.Context, controlID string) (ControlRow, bool, error) {
	controlID = strings.TrimSpace(controlID)
	if controlID == "" {
		return ControlRow{}, false, nil
	}
	var out ControlRow
	var observe, exec, reason string
	err := s.db.QueryRowContext(ctx, `SELECT control_id, call_id, work, op, ttl_ms,
		observe, exec, reason, created_at, updated_at
		FROM call_controls WHERE control_id = ?`, controlID).Scan(
		&out.ControlID, &out.CallID, &out.Work, &out.Op, &out.TTLMS,
		&observe, &exec, &reason, &out.CreatedAt, &out.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ControlRow{}, false, nil
	}
	if err != nil {
		return ControlRow{}, false, fmt.Errorf("读取 control %s: %w", controlID, err)
	}
	out.Observe, out.Exec, out.Reason = observe, exec, reason
	if observe != "" || exec != "" || reason != "" {
		out.Result, err = encodeControlResult(observe, exec, reason)
		if err != nil {
			return ControlRow{}, false, err
		}
	}
	return out, true, nil
}

// ---------- 常驻目录 ----------

// ListResident 列出某个 owner 名下的常驻调用（mode=resident，按 created_at 倒序）。
// owner 来自 works.owner_id（S-001 §9）；limit 默认 50、上限 200；
// cursor 是上一页最后一条 call id（下一页从它之后继续，空 = 第一页）。
func (s *Store) ListResident(ctx context.Context, owner string, limit int, cursor string) ([]CallRow, error) {
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return nil, errors.New("ListResident: owner 必填")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	query := selectCallSQL + ` WHERE c.mode = ? AND w.owner_id = ?`
	args := []any{CallModeResident, owner}
	if cursor = strings.TrimSpace(cursor); cursor != "" {
		// created_at 可能相同（同一纳秒），用 id 做稳定的二级游标。
		query += ` AND (c.created_at < (SELECT created_at FROM calls WHERE id = ?)
			OR (c.created_at = (SELECT created_at FROM calls WHERE id = ?) AND c.id < ?))`
		args = append(args, cursor, cursor, cursor)
	}
	query += ` ORDER BY c.created_at DESC, c.id DESC LIMIT ?`
	args = append(args, limit)
	return s.queryCalls(ctx, query, args...)
}

// ListCallsForReconcile 返回周期对账要看的 call：未关闭、且执行维不是终态（done / failed /
// cancelled 已是结果，不需要 owner 再回答）。只读；按 created_at 升序（老的先看）。
//
// 跑在 session 的周期循环里（reconcile.go），不改变任何状态——调用方自己决定怎么保守合并。
func (s *Store) ListCallsForReconcile(ctx context.Context) ([]CallRow, error) {
	return s.queryCalls(ctx, selectCallSQL+`
		WHERE c.observe_state != ? AND c.exec_state NOT IN (?, ?, ?)
		ORDER BY c.created_at ASC, c.id ASC`,
		CallObserveClosed, CallExecDone, CallExecFailed, CallExecCancelled)
}

func (s *Store) queryCalls(ctx context.Context, query string, args ...any) ([]CallRow, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("查询 calls: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]CallRow, 0, 16)
	for rows.Next() {
		row, err := scanCallRow(rows)
		if err != nil {
			return nil, fmt.Errorf("读取 calls 行: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历 calls: %w", err)
	}
	return out, nil
}

// isUniqueViolation 判断 SQLite 唯一约束错误（modernc.org/sqlite 没暴露稳定错误码，
// 这里只看驱动错误文本；判断范围仅用于把冲突翻成 ErrCallConflict）。
func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToUpper(err.Error()), "UNIQUE CONSTRAINT FAILED")
}
