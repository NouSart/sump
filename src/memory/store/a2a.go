package store

// 生产 A2A 队列（spec §6.8 work_messages / §17.1 A2A）：
// 正文只存一份（messages 行），work_messages 只存 body_ref 指针；
// claim/ack/nack 是队列状态机，nack 超过重试上限进 failed（dead-letter）。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/spv2"
)

const a2aSchema = `
CREATE TABLE IF NOT EXISTS work_messages (
  id TEXT PRIMARY KEY,
  work_id TEXT NOT NULL,
  from_kind TEXT NOT NULL,
  from_id TEXT NOT NULL,
  to_kind TEXT NOT NULL DEFAULT "",
  to_id TEXT NOT NULL DEFAULT "",
  method TEXT NOT NULL DEFAULT "message",
  body_ref INTEGER NOT NULL,
  state TEXT NOT NULL,
  lease_until TEXT NOT NULL DEFAULT "",
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT "",
  created_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_work_messages_claim ON work_messages(work_id, state, created_at);
`

// work_messages 的队列状态（spec §6.8）。
const (
	WorkMsgPending   = "pending"
	WorkMsgClaimed   = "claimed"
	WorkMsgDelivered = "delivered"
	WorkMsgFailed    = "failed"
	WorkMsgExpired   = "expired"
)

// WorkMessage 是一条 A2A 队列消息。
type WorkMessage struct {
	ID     string   `msgpack:"id" json:"id"`
	WorkID string   `msgpack:"work_id,omitempty" json:"work_id,omitempty"`
	From   spv2.Ref `msgpack:"from,omitempty" json:"from,omitempty"`
	To     spv2.Ref `msgpack:"to,omitempty" json:"to,omitempty"`
	Method string   `msgpack:"method,omitempty" json:"method,omitempty"`
	// BodyRef 指向 messages.id（正文只存一份）；Body 是读取视图，消费方不必再查库。
	BodyRef    int64  `msgpack:"body_ref,omitempty" json:"body_ref,omitempty"`
	Body       string `msgpack:"body,omitempty" json:"body,omitempty"`
	State      string `msgpack:"state" json:"state"`
	LeaseUntil string `msgpack:"lease_until,omitempty" json:"lease_until,omitempty"`
	Attempts   int    `msgpack:"attempts,omitempty" json:"attempts,omitempty"`
	LastError  string `msgpack:"last_error,omitempty" json:"last_error,omitempty"`
	CreatedAt  string `msgpack:"created_at,omitempty" json:"created_at,omitempty"`
}

// PostWorkMessage 入队一条 A2A 消息；终态 work 不允许再入队（conflict）。
func (s *Store) PostWorkMessage(ctx context.Context, workID string, from, to spv2.Ref, method, body string) (WorkMessage, error) {
	w, err := s.GetWork(ctx, workID)
	if err != nil {
		return WorkMessage{}, err
	}
	if spv2.IsTerminal(w.Status) {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeConflict, "work "+workID+" 已终态，不能再入队消息", false, nil)
	}
	if to.Kind == "" || to.ID == "" {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeInvalidArgument, "message.to 必填", false, nil)
	}
	if method == "" {
		method = "message"
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `INSERT INTO messages(conversation_id, role, content, kind, meta, seq, created_at, task_id)
		VALUES(?,?,?,?,?, (SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id=?), ?, "")`,
		workID, from.Kind+":"+from.ID, body, "a2a", method, workID, now)
	if err != nil {
		return WorkMessage{}, fmt.Errorf("A2A 正文落库: %w", err)
	}
	bodyRef, err := res.LastInsertId()
	if err != nil {
		return WorkMessage{}, err
	}
	id := spv2.NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_messages
		(id, work_id, from_kind, from_id, to_kind, to_id, method, body_ref, state, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?)`,
		id, workID, from.Kind, from.ID, to.Kind, to.ID, method, bodyRef, WorkMsgPending, now); err != nil {
		return WorkMessage{}, fmt.Errorf("A2A 消息入队: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return WorkMessage{}, err
	}
	return s.GetWorkMessage(ctx, id)
}

// ClaimWorkMessage 领取属于 actor 的一条 pending（或租约已过期的 claimed）消息。
// 没有可领的消息回 not_found，不返回空壳。
func (s *Store) ClaimWorkMessage(ctx context.Context, workID, actorKind, actorID string, lease time.Duration) (WorkMessage, error) {
	now := time.Now().UTC()
	ts := now.Format(time.RFC3339Nano)
	deadline := now.Add(lease).Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkMessage{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var m WorkMessage
	err = tx.QueryRowContext(ctx, `SELECT wm.id, wm.work_id, wm.from_kind, wm.from_id, wm.to_kind, wm.to_id,
			wm.method, wm.body_ref, wm.state, wm.lease_until, wm.attempts, wm.last_error, wm.created_at, m.content
		FROM work_messages wm JOIN messages m ON m.id = wm.body_ref
		WHERE wm.work_id = ? AND (wm.state = ? OR (wm.state = ? AND wm.lease_until != "" AND wm.lease_until < ?))
		AND (wm.to_id = "" OR wm.to_id = ?)
		ORDER BY wm.created_at, wm.id LIMIT 1`,
		workID, WorkMsgPending, WorkMsgClaimed, ts, actorID).Scan(
		&m.ID, &m.WorkID, &m.From.Kind, &m.From.ID, &m.To.Kind, &m.To.ID, &m.Method, &m.BodyRef,
		&m.State, &m.LeaseUntil, &m.Attempts, &m.LastError, &m.CreatedAt, &m.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeNotFound, "work "+workID+" 没有可领取的消息", false, nil)
	}
	if err != nil {
		return WorkMessage{}, fmt.Errorf("查询 A2A 队列: %w", err)
	}
	res, err := tx.ExecContext(ctx, `UPDATE work_messages SET state = ?, lease_until = ?, attempts = attempts + 1 WHERE id = ? AND state IN (?, ?)`,
		WorkMsgClaimed, deadline, m.ID, WorkMsgPending, WorkMsgClaimed)
	if err != nil {
		return WorkMessage{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeConflict, "消息已被其它 worker 领走", false, nil)
	}
	if err := tx.Commit(); err != nil {
		return WorkMessage{}, err
	}
	_ = actorKind
	return s.GetWorkMessage(ctx, m.ID)
}

// AckWorkMessage 把 claimed 消息标 delivered（终态）。
func (s *Store) AckWorkMessage(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE work_messages SET state = ?, lease_until = "" WHERE id = ? AND state = ?`,
		WorkMsgDelivered, id, WorkMsgClaimed)
	if err != nil {
		return fmt.Errorf("ack A2A 消息: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return spv2.ErrCode(spv2.CodeConflict, "消息 "+id+" 不在 claimed 状态", false, nil)
	}
	return nil
}

// NackWorkMessage 处理失败：未超重试上限回 pending，超过则进 failed（dead-letter）。
// 返回 retried=true 表示已重新入队。
func (s *Store) NackWorkMessage(ctx context.Context, id, reason string, maxAttempts int) (bool, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	m, err := s.GetWorkMessage(ctx, id)
	if err != nil {
		return false, err
	}
	if m.State != WorkMsgClaimed {
		return false, spv2.ErrCode(spv2.CodeConflict, "消息 "+id+" 不在 claimed 状态", false, nil)
	}
	state := WorkMsgPending
	if m.Attempts >= maxAttempts {
		state = WorkMsgFailed
	}
	res, err := s.db.ExecContext(ctx, `UPDATE work_messages SET state = ?, last_error = ?, lease_until = "" WHERE id = ? AND state = ?`,
		state, reason, id, WorkMsgClaimed)
	if err != nil {
		return false, fmt.Errorf("nack A2A 消息: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, spv2.ErrCode(spv2.CodeConflict, "消息 "+id+" 状态已变", false, nil)
	}
	return state == WorkMsgPending, nil
}

// GetWorkMessage 读一条队列消息。
func (s *Store) GetWorkMessage(ctx context.Context, id string) (WorkMessage, error) {
	var m WorkMessage
	err := s.db.QueryRowContext(ctx, `SELECT wm.id, wm.work_id, wm.from_kind, wm.from_id, wm.to_kind, wm.to_id,
		wm.method, wm.body_ref, wm.state, wm.lease_until, wm.attempts, wm.last_error, wm.created_at, m.content
		FROM work_messages wm JOIN messages m ON m.id = wm.body_ref WHERE wm.id = ?`, id).Scan(
		&m.ID, &m.WorkID, &m.From.Kind, &m.From.ID, &m.To.Kind, &m.To.ID, &m.Method, &m.BodyRef,
		&m.State, &m.LeaseUntil, &m.Attempts, &m.LastError, &m.CreatedAt, &m.Body)
	if errors.Is(err, sql.ErrNoRows) {
		return WorkMessage{}, spv2.ErrCode(spv2.CodeNotFound, "消息 "+id+" 不存在", false, nil)
	}
	if err != nil {
		return WorkMessage{}, fmt.Errorf("读取 A2A 消息: %w", err)
	}
	return m, nil
}

// Renew 续租：owner/runtime 在到期前/后刷新 lease，并把 stalled 拉回 running。
func (s *Store) Renew(ctx context.Context, id string, lease spv2.Lease) error {
	w, err := s.GetWork(ctx, id)
	if err != nil {
		return err
	}
	if spv2.IsTerminal(w.Status) {
		return spv2.ErrCode(spv2.CodeConflict, "work "+id+" 已终态，不能续租", false, nil)
	}
	if lease.DeadlineAt != "" {
		t, err := time.Parse(time.RFC3339, lease.DeadlineAt)
		if err != nil {
			return spv2.ErrCode(spv2.CodeInvalidArgument, "renew.deadline_at 不是 RFC3339", false, nil)
		}
		if t.Before(time.Now()) {
			return spv2.ErrCode(spv2.CodeInvalidArgument, "renew.deadline_at 不得改到过去", false, nil)
		}
	}
	set := []string{"updated_at = ?"}
	args := []any{time.Now().UTC().Format(time.RFC3339)}
	if lease.DeadlineAt != "" {
		set = append(set, "deadline_at = ?")
		args = append(args, lease.DeadlineAt)
	}
	if lease.MaxToolCalls > 0 {
		set = append(set, "max_tools = ?", "tools_left = ?")
		args = append(args, lease.MaxToolCalls, lease.MaxToolCalls)
	}
	if lease.MaxTokens > 0 {
		set = append(set, "max_tokens = ?", "tokens_left = ?")
		args = append(args, lease.MaxTokens, lease.MaxTokens)
	}
	if w.Status == spv2.WorkStalled {
		set = append(set, "status = ?")
		args = append(args, spv2.WorkRunning)
	}
	args = append(args, id)
	if _, err := s.db.ExecContext(ctx, "UPDATE works SET "+strings.Join(set, ", ")+" WHERE id = ?", args...); err != nil {
		return fmt.Errorf("renew work %s: %w", id, err)
	}
	return nil
}

// QueueDepth 返回各 state 的 A2A 消息总数（§12.3 queue_depth 的低基数来源）。
func (s *Store) QueueDepth(ctx context.Context) (map[string]int, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT state, COUNT(*) FROM work_messages GROUP BY state`)
	if err != nil {
		return nil, fmt.Errorf("统计 A2A 队列深度: %w", err)
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var state string
		var n int
		if err := rows.Scan(&state, &n); err != nil {
			return nil, err
		}
		out[state] = n
	}
	return out, rows.Err()
}
