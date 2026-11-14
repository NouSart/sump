package store

// work 账本（spec §6.8）：生产 memory 是 work 的唯一登记处。
// 表结构与参考实现同语义（states/lease 字段），但落在 memory 自己的库里，
// 不共享任何进程内状态。

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/NouSart/sump/spv2"
)

const workSchema = `
CREATE TABLE IF NOT EXISTS works (
  id TEXT PRIMARY KEY,
  root TEXT NOT NULL DEFAULT "",
  parent TEXT NOT NULL DEFAULT "",
  class TEXT NOT NULL DEFAULT "request",
  owner_kind TEXT NOT NULL DEFAULT "",
  owner_id TEXT NOT NULL DEFAULT "",
  agent_kind TEXT NOT NULL DEFAULT "",
  agent_id TEXT NOT NULL DEFAULT "",
  entry_kind TEXT NOT NULL DEFAULT "",
  entry_id TEXT NOT NULL DEFAULT "",
  goal TEXT NOT NULL DEFAULT "",
  status TEXT NOT NULL,
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at TEXT NOT NULL DEFAULT "",
  tools_left INTEGER NOT NULL,
  tokens_left INTEGER NOT NULL,
  approvals_left INTEGER NOT NULL,
  children_left INTEGER NOT NULL,
  depth_left INTEGER NOT NULL,
  max_tools INTEGER NOT NULL,
  max_tokens INTEGER NOT NULL,
  max_approvals INTEGER NOT NULL,
  max_children INTEGER NOT NULL,
  max_depth INTEGER NOT NULL,
  deadline_at TEXT NOT NULL DEFAULT ""
);
CREATE TABLE IF NOT EXISTS work_participants (
  work_id TEXT NOT NULL,
  kind TEXT NOT NULL,
  id TEXT NOT NULL,
  role TEXT NOT NULL DEFAULT "",
  status TEXT NOT NULL DEFAULT "",
  PRIMARY KEY (work_id, kind, id)
);
`

// EnsureWork 幂等登记一个 work；缺省字段按 spec 默认值补齐。
func (s *Store) EnsureWork(ctx context.Context, w spv2.Work) (spv2.Work, error) {
	now := time.Now().UTC()
	if w.Root == "" {
		w.Root = w.ID
	}
	if w.Class == "" {
		w.Class = spv2.ClassRequest
	}
	if w.Status == "" {
		w.Status = spv2.WorkOpen
	}
	def := spv2.DefaultLease(now)
	if w.Lease.MaxToolCalls == 0 {
		w.Lease.MaxToolCalls = def.MaxToolCalls
	}
	if w.Lease.MaxTokens == 0 {
		w.Lease.MaxTokens = def.MaxTokens
	}
	if w.Lease.MaxApprovals == 0 {
		w.Lease.MaxApprovals = def.MaxApprovals
	}
	if w.Lease.MaxChildren == 0 {
		w.Lease.MaxChildren = def.MaxChildren
	}
	if w.Lease.MaxDepth == 0 {
		w.Lease.MaxDepth = def.MaxDepth
	}
	if w.Lease.DeadlineAt == "" {
		w.Lease.DeadlineAt = def.DeadlineAt
	}
	if w.CreatedAt == "" {
		w.CreatedAt = now.Format(time.RFC3339)
	}
	w.UpdatedAt = w.CreatedAt
	if err := w.Validate(); err != nil {
		return spv2.Work{}, spv2.ErrCode(spv2.CodeInvalidArgument, "work: "+err.Error(), false, nil)
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO works
		(id, root, parent, class, owner_kind, owner_id, agent_kind, agent_id, entry_kind, entry_id,
		 goal, status, created_at, updated_at, closed_at,
		 tools_left, tokens_left, approvals_left, children_left, depth_left,
		 max_tools, max_tokens, max_approvals, max_children, max_depth, deadline_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		w.ID, w.Root, w.Parent, w.Class, w.Owner.Kind, w.Owner.ID, w.Agent.Kind, w.Agent.ID,
		w.Entry.Kind, w.Entry.ID, w.Goal, w.Status, w.CreatedAt, w.UpdatedAt, "",
		w.Lease.MaxToolCalls, w.Lease.MaxTokens, w.Lease.MaxApprovals, w.Lease.MaxChildren, w.Lease.MaxDepth,
		w.Lease.MaxToolCalls, w.Lease.MaxTokens, w.Lease.MaxApprovals, w.Lease.MaxChildren, w.Lease.MaxDepth,
		w.Lease.DeadlineAt)
	if err != nil {
		return spv2.Work{}, fmt.Errorf("登记 work %s: %w", w.ID, err)
	}
	return s.GetWork(ctx, w.ID)
}

// GetWork 读取 work（含 lease 余额与参与者）。
func (s *Store) GetWork(ctx context.Context, id string) (spv2.Work, error) {
	var w spv2.Work
	var tools, tokens, approvals, children, depth int
	var maxTools, maxTokens, maxApprovals, maxChildren, maxDepth int
	err := s.db.QueryRowContext(ctx, `SELECT id, root, parent, class, owner_kind, owner_id,
		agent_kind, agent_id, entry_kind, entry_id, goal, status, created_at, updated_at, closed_at,
		tools_left, tokens_left, approvals_left, children_left, depth_left,
		max_tools, max_tokens, max_approvals, max_children, max_depth, deadline_at
		FROM works WHERE id = ?`, id).Scan(
		&w.ID, &w.Root, &w.Parent, &w.Class, &w.Owner.Kind, &w.Owner.ID,
		&w.Agent.Kind, &w.Agent.ID, &w.Entry.Kind, &w.Entry.ID, &w.Goal, &w.Status,
		&w.CreatedAt, &w.UpdatedAt, &w.ClosedAt,
		&tools, &tokens, &approvals, &children, &depth,
		&maxTools, &maxTokens, &maxApprovals, &maxChildren, &maxDepth, &w.Lease.DeadlineAt)
	if errors.Is(err, sql.ErrNoRows) {
		return spv2.Work{}, spv2.ErrCode(spv2.CodeNotFound, "work "+id+" 不存在", false, nil)
	}
	if err != nil {
		return spv2.Work{}, fmt.Errorf("读取 work %s: %w", id, err)
	}
	w.Lease.MaxToolCalls = maxTools
	w.Lease.MaxTokens = maxTokens
	w.Lease.MaxApprovals = maxApprovals
	w.Lease.MaxChildren = maxChildren
	w.Lease.MaxDepth = maxDepth
	parts, err := s.participants(ctx, id)
	if err != nil {
		return spv2.Work{}, err
	}
	w.Participants = parts
	return w, nil
}

func (s *Store) participants(ctx context.Context, id string) ([]spv2.Participant, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT kind, id, role, status FROM work_participants WHERE work_id = ? ORDER BY kind, id`, id)
	if err != nil {
		return nil, fmt.Errorf("读取 work %s 参与者: %w", id, err)
	}
	defer rows.Close()
	var out []spv2.Participant
	for rows.Next() {
		var p spv2.Participant
		if err := rows.Scan(&p.Actor.Kind, &p.Actor.ID, &p.Role, &p.Status); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// AddParticipant 幂等加入参与者。
func (s *Store) AddParticipant(ctx context.Context, workID string, p spv2.Participant) error {
	if p.Actor.Kind == "" || p.Actor.ID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "participant.actor 必填", false, nil)
	}
	if _, err := s.GetWork(ctx, workID); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO work_participants (work_id, kind, id, role, status) VALUES (?,?,?,?,?)`,
		workID, p.Actor.Kind, p.Actor.ID, p.Role, p.Status)
	if err != nil {
		return fmt.Errorf("登记 work %s 参与者: %w", workID, err)
	}
	return nil
}

// UpdateStatus 按 §6.3 状态机改状态；非法转移回 conflict。
func (s *Store) UpdateStatus(ctx context.Context, id, status string) error {
	w, err := s.GetWork(ctx, id)
	if err != nil {
		return err
	}
	if !spv2.CanTransition(w.Status, status) {
		return spv2.ErrCode(spv2.CodeConflict, "work "+id+" 非法状态转移: "+w.Status+" → "+status, false, nil)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	closed := ""
	if spv2.IsTerminal(status) {
		closed = now
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE works SET status = ?, updated_at = ?, closed_at = CASE WHEN ? = "" THEN closed_at ELSE ? END WHERE id = ?`,
		status, now, closed, closed, id); err != nil {
		return fmt.Errorf("更新 work %s 状态: %w", id, err)
	}
	return nil
}

// Charge 原子扣减租约；预算不足、过期、已 stalled 分别回 §8 错误码。
func (s *Store) Charge(ctx context.Context, id string, c spv2.Charge) (spv2.Remaining, error) {
	rem, err := s.Remaining(ctx, id)
	if err != nil {
		return spv2.Remaining{}, err
	}
	w, err := s.GetWork(ctx, id)
	if err != nil {
		return spv2.Remaining{}, err
	}
	if w.Status == spv2.WorkStalled {
		return rem, spv2.ErrCode(spv2.CodeStalled, "work "+id+" 已 stalled（先 renew/resume）", false, nil)
	}
	if t, ok := parseTS(rem.DeadlineAt); ok && time.Now().After(t) {
		return rem, spv2.ErrCode(spv2.CodeDeadlineExceeded, "work "+id+" deadline exceeded", true, nil)
	}
	if c.ToolCalls > rem.ToolCalls || c.Tokens > rem.Tokens || c.Approvals > rem.Approvals || c.Children > rem.Children || c.Depth > rem.Depth {
		return rem, spv2.ErrCode(spv2.CodeResourceExhausted, "work "+id+" lease 预算不足", true, nil)
	}
	res, err := s.db.ExecContext(ctx, `UPDATE works SET tools_left = tools_left - ?, tokens_left = tokens_left - ?,
		approvals_left = approvals_left - ?, children_left = children_left - ?, depth_left = depth_left - ?
		WHERE id = ? AND tools_left >= ? AND tokens_left >= ? AND approvals_left >= ? AND children_left >= ? AND depth_left >= ?`,
		c.ToolCalls, c.Tokens, c.Approvals, c.Children, c.Depth, id,
		c.ToolCalls, c.Tokens, c.Approvals, c.Children, c.Depth)
	if err != nil {
		return spv2.Remaining{}, fmt.Errorf("扣减 work %s 租约: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return spv2.Remaining{}, err
	}
	if n == 0 {
		return s.Remaining(ctx, id)
	}
	return s.Remaining(ctx, id)
}

// Release 归还租约（不超过初始上限）。
func (s *Store) Release(ctx context.Context, id string, c spv2.Charge) error {
	if _, err := s.GetWork(ctx, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `UPDATE works SET
		tools_left = MIN(tools_left + ?, max_tools),
		tokens_left = MIN(tokens_left + ?, max_tokens),
		approvals_left = MIN(approvals_left + ?, max_approvals),
		children_left = MIN(children_left + ?, max_children),
		depth_left = MIN(depth_left + ?, max_depth) WHERE id = ?`,
		c.ToolCalls, c.Tokens, c.Approvals, c.Children, c.Depth, id)
	if err != nil {
		return fmt.Errorf("归还 work %s 租约: %w", id, err)
	}
	return nil
}

// Remaining 读租约余额。
func (s *Store) Remaining(ctx context.Context, id string) (spv2.Remaining, error) {
	var rem spv2.Remaining
	err := s.db.QueryRowContext(ctx, `SELECT deadline_at, tools_left, tokens_left, approvals_left, children_left, depth_left FROM works WHERE id = ?`, id).Scan(
		&rem.DeadlineAt, &rem.ToolCalls, &rem.Tokens, &rem.Approvals, &rem.Children, &rem.Depth)
	if errors.Is(err, sql.ErrNoRows) {
		return spv2.Remaining{}, spv2.ErrCode(spv2.CodeNotFound, "work "+id+" 不存在", false, nil)
	}
	if err != nil {
		return spv2.Remaining{}, fmt.Errorf("读取 work %s 租约: %w", id, err)
	}
	return rem, nil
}

func parseTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, s)
	return t, err == nil
}

// SweepExpired 执行 spec §6.9 的第 1 条维护规则：deadline 已过且未终态的 work → stalled。
// sweep 只做维护、不派活（memory 是账本 owner，不是中央调度器）。
func (s *Store) SweepExpired(ctx context.Context, now time.Time) (int, error) {
	ts := now.UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `UPDATE works SET status = ?, updated_at = ?
		WHERE status IN (?, ?, ?) AND deadline_at != "" AND deadline_at < ?`,
		spv2.WorkStalled, ts, spv2.WorkOpen, spv2.WorkRunning, spv2.WorkWaiting, ts)
	if err != nil {
		return 0, fmt.Errorf("sweep 过期 work: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}
