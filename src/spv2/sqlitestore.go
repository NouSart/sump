package spv2

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// SQLiteStore 是 WorkStore 的 SQLite 实现（memory 的权威 work 账本）。
//
// 表结构参考 docs/concepts/spv2.md §6.8。除 §6.8 列出的剩余额度外，还额外保存
// lease 的初始上限（max_*）：Release 归还时据此封顶，GetWork 也能还原 Work.Lease。
//
// 写入事务都走单连接（SetMaxOpenConns(1)），把 SQLite 的单写者语义收敛在进程内；
// busy_timeout 则用于跨进程共存时的短暂锁冲突。
type SQLiteStore struct {
	db       *sql.DB
	maxTries int
	msgTTL   time.Duration
}

var _ WorkStore = (*SQLiteStore)(nil)

const (
	sqliteDefaultMaxTries = 3
	sqliteDefaultMsgTTL   = 24 * time.Hour
)

const sqliteSchema = `
CREATE TABLE IF NOT EXISTS works (
  id TEXT PRIMARY KEY,
  parent TEXT NOT NULL DEFAULT '',
  root TEXT NOT NULL,
  class TEXT NOT NULL,
  owner_kind TEXT NOT NULL,
  owner_id TEXT NOT NULL,
  owner_authority TEXT NOT NULL DEFAULT '',
  owner_tenant TEXT NOT NULL DEFAULT '',
  agent_kind TEXT NOT NULL,
  agent_id TEXT NOT NULL,
  agent_authority TEXT NOT NULL DEFAULT '',
  agent_tenant TEXT NOT NULL DEFAULT '',
  entry_kind TEXT NOT NULL,
  entry_id TEXT NOT NULL,
  entry_authority TEXT NOT NULL DEFAULT '',
  entry_tenant TEXT NOT NULL DEFAULT '',
  goal TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  deadline_at TEXT NOT NULL DEFAULT '',
  max_tool_calls INTEGER NOT NULL DEFAULT 0,
  max_tokens INTEGER NOT NULL DEFAULT 0,
  max_approvals INTEGER NOT NULL DEFAULT 0,
  max_children INTEGER NOT NULL DEFAULT 0,
  max_depth INTEGER NOT NULL DEFAULT 0,
  tools_left INTEGER NOT NULL DEFAULT 0,
  tokens_left INTEGER NOT NULL DEFAULT 0,
  approvals_left INTEGER NOT NULL DEFAULT 0,
  children_left INTEGER NOT NULL DEFAULT 0,
  depth_left INTEGER NOT NULL DEFAULT 0,
  progress_hash TEXT NOT NULL DEFAULT '',
  progress_at TEXT NOT NULL DEFAULT '',
  policy_json TEXT NOT NULL DEFAULT 'null',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  closed_at TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS work_participants (
  work_id TEXT NOT NULL,
  actor_kind TEXT NOT NULL,
  actor_id TEXT NOT NULL,
  actor_authority TEXT NOT NULL DEFAULT '',
  actor_tenant TEXT NOT NULL DEFAULT '',
  role TEXT NOT NULL,
  status TEXT NOT NULL,
  joined_at TEXT NOT NULL,
  PRIMARY KEY (work_id, actor_kind, actor_id)
);

CREATE TABLE IF NOT EXISTS work_messages (
  id TEXT PRIMARY KEY,
  work_id TEXT NOT NULL,
  from_kind TEXT NOT NULL,
  from_id TEXT NOT NULL,
  from_authority TEXT NOT NULL DEFAULT '',
  from_tenant TEXT NOT NULL DEFAULT '',
  to_kind TEXT NOT NULL,
  to_id TEXT NOT NULL,
  to_authority TEXT NOT NULL DEFAULT '',
  to_tenant TEXT NOT NULL DEFAULT '',
  method TEXT NOT NULL,
  reply_to TEXT NOT NULL DEFAULT '',
  -- 参考实现直接内联正文（spv2 模块没有共享 messages 表，故不存 body_ref）。
  body BLOB,
  state TEXT NOT NULL,
  lease_until TEXT NOT NULL DEFAULT '',
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_works_parent ON works(parent);
CREATE INDEX IF NOT EXISTS idx_work_messages_claim ON work_messages(work_id, state, created_at, id);
`

// NewSQLiteStore 打开（或创建）SQLite 库、设置 busy_timeout 并建表。
func NewSQLiteStore(path string) (*SQLiteStore, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建 sqlite 目录 %s: %w", dir, err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开 sqlite %s: %w", path, err)
	}
	// SQLite 单写者：串行化连接，避免同进程内并发写锁冲突。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("设置 sqlite busy_timeout: %w", err)
	}
	if _, err := db.Exec(sqliteSchema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("建表 sqlite %s: %w", path, err)
	}
	return &SQLiteStore{db: db, maxTries: sqliteDefaultMaxTries, msgTTL: sqliteDefaultMsgTTL}, nil
}

// Close 关闭数据库。
func (s *SQLiteStore) Close() error { return s.db.Close() }

// sqliteTSLayout 是入库时间戳的固定宽度格式，保证 TEXT 排序与时间先后一致。
const sqliteTSLayout = "2006-01-02T15:04:05.000000000Z07:00"

func sqliteTS(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(sqliteTSLayout)
}

func sqliteParseTS(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}

// sqliteQuerier 抽象 *sql.DB 与 *sql.Tx，事务内复用同一套读取逻辑。
type sqliteQuerier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

type sqliteScanner interface {
	Scan(dest ...any) error
}

const sqliteWorkColumns = `id, parent, root, class,
	owner_kind, owner_id, owner_authority, owner_tenant,
	agent_kind, agent_id, agent_authority, agent_tenant,
	entry_kind, entry_id, entry_authority, entry_tenant,
	goal, status, deadline_at,
	max_tool_calls, max_tokens, max_approvals, max_children, max_depth,
	tools_left, tokens_left, approvals_left, children_left, depth_left,
	policy_json, created_at, updated_at, closed_at`

const sqliteMessageColumns = `id, work_id,
	from_kind, from_id, from_authority, from_tenant,
	to_kind, to_id, to_authority, to_tenant,
	method, reply_to, body, state, lease_until, attempts, last_error, created_at`

// EnsureWork 幂等创建 work；已存在则原样返回（不覆盖）。
func (s *SQLiteStore) EnsureWork(ctx context.Context, w Work) (Work, error) {
	now := time.Now()
	ensureDefaults(&w, now)
	if err := w.Validate(); err != nil {
		return Work{}, err
	}
	policy, err := json.Marshal(w.Policy)
	if err != nil {
		return Work{}, fmt.Errorf("序列化 work %s 的 policy: %w", w.ID, err)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Work{}, err
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO works (
		id, parent, root, class,
		owner_kind, owner_id, owner_authority, owner_tenant,
		agent_kind, agent_id, agent_authority, agent_tenant,
		entry_kind, entry_id, entry_authority, entry_tenant,
		goal, status, deadline_at,
		max_tool_calls, max_tokens, max_approvals, max_children, max_depth,
		tools_left, tokens_left, approvals_left, children_left, depth_left,
		progress_hash, progress_at, policy_json, created_at, updated_at, closed_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Parent, w.Root, w.Class,
		w.Owner.Kind, w.Owner.ID, w.Owner.Authority, w.Owner.Tenant,
		w.Agent.Kind, w.Agent.ID, w.Agent.Authority, w.Agent.Tenant,
		w.Entry.Kind, w.Entry.ID, w.Entry.Authority, w.Entry.Tenant,
		w.Goal, w.Status, w.Lease.DeadlineAt,
		w.Lease.MaxToolCalls, w.Lease.MaxTokens, w.Lease.MaxApprovals, w.Lease.MaxChildren, w.Lease.MaxDepth,
		w.Lease.MaxToolCalls, w.Lease.MaxTokens, w.Lease.MaxApprovals, w.Lease.MaxChildren, w.Lease.MaxDepth,
		"", "", string(policy), w.CreatedAt, w.UpdatedAt, w.ClosedAt,
	)
	if err != nil {
		return Work{}, fmt.Errorf("写入 work %s: %w", w.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Work{}, fmt.Errorf("写入 work %s: %w", w.ID, err)
	}
	if n == 0 {
		got, err := sqliteLoadWork(ctx, tx, w.ID)
		if err != nil {
			return Work{}, err
		}
		if err := tx.Commit(); err != nil {
			return Work{}, err
		}
		return got, nil
	}
	for _, p := range w.Participants {
		if err := sqliteUpsertParticipant(ctx, tx, w.ID, p, now); err != nil {
			return Work{}, err
		}
	}
	if err := tx.Commit(); err != nil {
		return Work{}, err
	}
	return copyWork(w), nil
}

// GetWork 返回 work。
func (s *SQLiteStore) GetWork(ctx context.Context, id string) (Work, error) {
	return sqliteLoadWork(ctx, s.db, id)
}

// UpdateStatus 按状态机更新 work 状态；done 前检查未完成消息与 child work。
func (s *SQLiteStore) UpdateStatus(ctx context.Context, id, status string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var current string
	err = tx.QueryRowContext(ctx, "SELECT status FROM works WHERE id = ?", id).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkNotFound
	}
	if err != nil {
		return fmt.Errorf("读取 work %s 的状态: %w", id, err)
	}
	if current == status {
		return nil
	}
	if !CanTransition(current, status) {
		return ErrCode(CodeConflict, fmt.Sprintf("work %s 不能从 %s 转到 %s", id, current, status), false, nil)
	}
	if status == WorkDone {
		var n int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM work_messages WHERE work_id = ? AND state IN (?, ?)",
			id, MessagePending, MessageClaimed).Scan(&n); err != nil {
			return fmt.Errorf("检查 work %s 的 A2A 队列: %w", id, err)
		}
		if n > 0 {
			return ErrCode(CodeConflict, fmt.Sprintf("work %s 还有未完成的 A2A 消息", id), false, nil)
		}
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM works WHERE parent = ? AND status NOT IN (?, ?, ?)",
			id, WorkDone, WorkFailed, WorkCancelled).Scan(&n); err != nil {
			return fmt.Errorf("检查 work %s 的 child work: %w", id, err)
		}
		if n > 0 {
			return ErrCode(CodeConflict, fmt.Sprintf("work %s 还有未完成的 child work", id), false, nil)
		}
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	closedAt := ""
	if IsTerminal(status) {
		closedAt = updatedAt
	}
	if _, err := tx.ExecContext(ctx, "UPDATE works SET status = ?, updated_at = ?, closed_at = ? WHERE id = ?",
		status, updatedAt, closedAt, id); err != nil {
		return fmt.Errorf("更新 work %s 的状态: %w", id, err)
	}
	return tx.Commit()
}

// AddParticipant 添加/更新参与者。
func (s *SQLiteStore) AddParticipant(ctx context.Context, workID string, p Participant) error {
	if p.Role == "" {
		p.Role = "member"
	}
	if p.Status == "" {
		p.Status = "active"
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM works WHERE id = ?", workID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkNotFound
	}
	if err != nil {
		return fmt.Errorf("读取 work %s: %w", workID, err)
	}
	now := time.Now().UTC()
	if err := sqliteUpsertParticipant(ctx, tx, workID, p, now); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "UPDATE works SET updated_at = ? WHERE id = ?",
		now.Format(time.RFC3339), workID); err != nil {
		return fmt.Errorf("更新 work %s: %w", workID, err)
	}
	return tx.Commit()
}

// Renew 续租：只增加预算/延后 deadline，不减少已用余额；终态 work 拒绝。
func (s *SQLiteStore) Renew(ctx context.Context, workID string, lease Lease) (Work, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Work{}, err
	}
	defer func() { _ = tx.Rollback() }()

	w, err := sqliteLoadWork(ctx, tx, workID)
	if err != nil {
		return Work{}, err
	}
	if IsTerminal(w.Status) {
		return Work{}, ErrCode(CodeConflict, "终态 work 不能续租", false, nil)
	}
	rem, err := sqliteLoadRemaining(ctx, tx, workID)
	if err != nil {
		return Work{}, err
	}
	if lease.DeadlineAt != "" {
		old, _ := time.Parse(time.RFC3339, w.Lease.DeadlineAt)
		nw, err := time.Parse(time.RFC3339, lease.DeadlineAt)
		if err != nil {
			return Work{}, ErrCode(CodeInvalidArgument, "lease.deadline_at 不是 RFC3339", false, nil)
		}
		if nw.After(old) {
			w.Lease.DeadlineAt = lease.DeadlineAt
		}
	}
	// 新上限高于旧上限时，把差额补进剩余额度；否则保持原值。
	raise := func(oldMax, newMax int) (int, int) {
		if newMax > oldMax {
			return newMax, newMax - oldMax
		}
		return oldMax, 0
	}
	var addTools, addTokens, addApprovals, addChildren, addDepth int
	w.Lease.MaxToolCalls, addTools = raise(w.Lease.MaxToolCalls, lease.MaxToolCalls)
	w.Lease.MaxTokens, addTokens = raise(w.Lease.MaxTokens, lease.MaxTokens)
	w.Lease.MaxApprovals, addApprovals = raise(w.Lease.MaxApprovals, lease.MaxApprovals)
	w.Lease.MaxChildren, addChildren = raise(w.Lease.MaxChildren, lease.MaxChildren)
	w.Lease.MaxDepth, addDepth = raise(w.Lease.MaxDepth, lease.MaxDepth)
	rem.ToolCalls += addTools
	rem.Tokens += addTokens
	rem.Approvals += addApprovals
	rem.Children += addChildren
	rem.Depth += addDepth

	w.UpdatedAt = time.Now().UTC().Format(time.RFC3339)
	if _, err := tx.ExecContext(ctx, "UPDATE works SET deadline_at = ?, max_tool_calls = ?, max_tokens = ?, max_approvals = ?, max_children = ?, max_depth = ?, tools_left = ?, tokens_left = ?, approvals_left = ?, children_left = ?, depth_left = ?, updated_at = ? WHERE id = ?",
		w.Lease.DeadlineAt, w.Lease.MaxToolCalls, w.Lease.MaxTokens, w.Lease.MaxApprovals, w.Lease.MaxChildren, w.Lease.MaxDepth,
		rem.ToolCalls, rem.Tokens, rem.Approvals, rem.Children, rem.Depth, w.UpdatedAt, workID); err != nil {
		return Work{}, fmt.Errorf("续租 work %s: %w", workID, err)
	}
	if err := tx.Commit(); err != nil {
		return Work{}, err
	}
	return w, nil
}

// Charge 在事务内原子扣减租约；预算不足返回 ErrLeaseExhausted，过期返回 deadline_exceeded。
func (s *SQLiteStore) Charge(ctx context.Context, workID string, c Charge) (Remaining, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Remaining{}, err
	}
	defer func() { _ = tx.Rollback() }()

	rem, err := sqliteLoadRemaining(ctx, tx, workID)
	if err != nil {
		return Remaining{}, err
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM works WHERE id = ?`, workID).Scan(&status); err != nil {
		return Remaining{}, fmt.Errorf("读 work %s 状态: %w", workID, err)
	}
	if status == WorkStalled {
		return rem, ErrCode(CodeStalled, "work "+workID+" 已 stalled（先 renew/resume）", false, nil)
	}
	if t, ok := sqliteParseTS(rem.DeadlineAt); ok && time.Now().After(t) {
		return rem, ErrCode(CodeDeadlineExceeded, "work "+workID+" deadline exceeded", true, nil)
	}
	if c.ToolCalls > rem.ToolCalls || c.Tokens > rem.Tokens || c.Approvals > rem.Approvals ||
		c.Children > rem.Children || c.Depth > rem.Depth {
		return rem, ErrLeaseExhausted
	}
	res, err := tx.ExecContext(ctx, `UPDATE works SET
		tools_left = tools_left - ?, tokens_left = tokens_left - ?,
		approvals_left = approvals_left - ?, children_left = children_left - ?, depth_left = depth_left - ?
		WHERE id = ? AND tools_left >= ? AND tokens_left >= ? AND approvals_left >= ? AND children_left >= ? AND depth_left >= ?`,
		c.ToolCalls, c.Tokens, c.Approvals, c.Children, c.Depth, workID,
		c.ToolCalls, c.Tokens, c.Approvals, c.Children, c.Depth)
	if err != nil {
		return Remaining{}, fmt.Errorf("扣减 work %s 的租约: %w", workID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Remaining{}, fmt.Errorf("扣减 work %s 的租约: %w", workID, err)
	}
	if n == 0 {
		// 并发写者抢先改了余额：按当前快照给出明确的错误。
		cur, err := sqliteLoadRemaining(ctx, tx, workID)
		if err != nil {
			return Remaining{}, err
		}
		if t, ok := sqliteParseTS(cur.DeadlineAt); ok && time.Now().After(t) {
			return cur, ErrCode(CodeDeadlineExceeded, "work "+workID+" deadline exceeded", true, nil)
		}
		return cur, ErrLeaseExhausted
	}
	next, err := sqliteLoadRemaining(ctx, tx, workID)
	if err != nil {
		return Remaining{}, err
	}
	if err := tx.Commit(); err != nil {
		return Remaining{}, err
	}
	return next, nil
}

// Release 归还租约（按初始上限封顶）。
func (s *SQLiteStore) Release(ctx context.Context, workID string, c Charge) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var rem Remaining
	var max Lease
	err = tx.QueryRowContext(ctx, `SELECT deadline_at, tools_left, tokens_left, approvals_left, children_left, depth_left,
		max_tool_calls, max_tokens, max_approvals, max_children, max_depth
		FROM works WHERE id = ?`, workID).Scan(
		&rem.DeadlineAt, &rem.ToolCalls, &rem.Tokens, &rem.Approvals, &rem.Children, &rem.Depth,
		&max.MaxToolCalls, &max.MaxTokens, &max.MaxApprovals, &max.MaxChildren, &max.MaxDepth)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrWorkNotFound
	}
	if err != nil {
		return fmt.Errorf("读取 work %s 的租约: %w", workID, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE works SET
		tools_left = ?, tokens_left = ?, approvals_left = ?, children_left = ?, depth_left = ?
		WHERE id = ?`,
		min(rem.ToolCalls+c.ToolCalls, max.MaxToolCalls),
		min(rem.Tokens+c.Tokens, max.MaxTokens),
		min(rem.Approvals+c.Approvals, max.MaxApprovals),
		min(rem.Children+c.Children, max.MaxChildren),
		min(rem.Depth+c.Depth, max.MaxDepth),
		workID); err != nil {
		return fmt.Errorf("归还 work %s 的租约: %w", workID, err)
	}
	return tx.Commit()
}

// Remaining 返回租约余额。
func (s *SQLiteStore) Remaining(ctx context.Context, workID string) (Remaining, error) {
	return sqliteLoadRemaining(ctx, s.db, workID)
}

// PostMessage 入队一条 A2A 消息。
func (s *SQLiteStore) PostMessage(ctx context.Context, m QueuedMessage) (string, error) {
	if m.WorkID == "" || m.Method == "" {
		return "", ErrCode(CodeInvalidArgument, "queued message 需要 work_id/method", false, nil)
	}
	if m.ID == "" {
		m.ID = NewID()
	}
	if m.State == "" {
		m.State = MessagePending
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now().UTC()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM works WHERE id = ?", m.WorkID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrWorkNotFound
	}
	if err != nil {
		return "", fmt.Errorf("读取 work %s: %w", m.WorkID, err)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO work_messages (
		id, work_id,
		from_kind, from_id, from_authority, from_tenant,
		to_kind, to_id, to_authority, to_tenant,
		method, reply_to, body, state, lease_until, attempts, last_error, created_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		m.ID, m.WorkID,
		m.From.Kind, m.From.ID, m.From.Authority, m.From.Tenant,
		m.To.Kind, m.To.ID, m.To.Authority, m.To.Tenant,
		m.Method, m.ReplyTo, m.Body, m.State, sqliteTS(m.LeaseUntil), m.Attempts, m.LastError, sqliteTS(m.CreatedAt),
	); err != nil {
		return "", fmt.Errorf("写入 message %s: %w", m.ID, err)
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return m.ID, nil
}

// ClaimMessage 领取最老的一条 pending 消息，并设置 lease_until。
func (s *SQLiteStore) ClaimMessage(ctx context.Context, workID string, lease time.Duration) (*QueuedMessage, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRowContext(ctx, "SELECT 1 FROM works WHERE id = ?", workID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrWorkNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("读取 work %s: %w", workID, err)
	}
	row := tx.QueryRowContext(ctx, "SELECT "+sqliteMessageColumns+" FROM work_messages WHERE work_id = ? AND state = ? ORDER BY created_at, id LIMIT 1", workID, MessagePending)
	m, err := sqliteScanMessage(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 work %s 的 pending 消息: %w", workID, err)
	}
	now := time.Now().UTC()
	leaseUntil := now.Add(lease)
	res, err := tx.ExecContext(ctx, "UPDATE work_messages SET state = ?, lease_until = ?, attempts = attempts + 1 WHERE id = ? AND state = ?",
		MessageClaimed, sqliteTS(leaseUntil), m.ID, MessagePending)
	if err != nil {
		return nil, fmt.Errorf("领取 message %s: %w", m.ID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("领取 message %s: %w", m.ID, err)
	}
	if n == 0 {
		return nil, nil
	}
	m.State = MessageClaimed
	m.LeaseUntil = leaseUntil
	m.Attempts++
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &m, nil
}

// AckMessage 标记消息已投递。
func (s *SQLiteStore) AckMessage(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, "UPDATE work_messages SET state = ?, lease_until = '' WHERE id = ?",
		MessageDelivered, id)
	if err != nil {
		return fmt.Errorf("确认 message %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("确认 message %s: %w", id, err)
	}
	if n == 0 {
		return ErrCode(CodeNotFound, "message not found: "+id, false, nil)
	}
	return nil
}

// NackMessage 记录错误；达到 maxTries 后转 failed，否则转回 pending。
func (s *SQLiteStore) NackMessage(ctx context.Context, id, errMsg string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var attempts int
	err = tx.QueryRowContext(ctx, "SELECT attempts FROM work_messages WHERE id = ?", id).Scan(&attempts)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrCode(CodeNotFound, "message not found: "+id, false, nil)
	}
	if err != nil {
		return fmt.Errorf("读取 message %s: %w", id, err)
	}
	state := MessagePending
	if attempts >= s.maxTries {
		state = MessageFailed
	}
	if _, err := tx.ExecContext(ctx, "UPDATE work_messages SET state = ?, lease_until = '', last_error = ? WHERE id = ?",
		state, errMsg, id); err != nil {
		return fmt.Errorf("更新 message %s: %w", id, err)
	}
	return tx.Commit()
}

// QueueDepth 返回某 work 的 pending+claimed 数。
func (s *SQLiteStore) QueueDepth(ctx context.Context, workID string) (int, error) {
	var one int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM works WHERE id = ?", workID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrWorkNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("读取 work %s: %w", workID, err)
	}
	var n int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM work_messages WHERE work_id = ? AND state IN (?, ?)",
		workID, MessagePending, MessageClaimed).Scan(&n); err != nil {
		return 0, fmt.Errorf("统计 work %s 的队列: %w", workID, err)
	}
	return n, nil
}

// Sweep 修复过期 deadline、租约与待投递消息。
func (s *SQLiteStore) Sweep(ctx context.Context, now time.Time) (SweepResult, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return SweepResult{}, err
	}
	defer func() { _ = tx.Rollback() }()

	// 先收集再更新：同一连接上不边读 rows 边写。
	var stalled []string
	rows, err := tx.QueryContext(ctx, `SELECT id, status, deadline_at FROM works
		WHERE deadline_at <> '' AND status NOT IN (?, ?, ?)`, WorkDone, WorkFailed, WorkCancelled)
	if err != nil {
		return SweepResult{}, fmt.Errorf("扫描 work deadline: %w", err)
	}
	for rows.Next() {
		var id, status, deadline string
		if err := rows.Scan(&id, &status, &deadline); err != nil {
			_ = rows.Close()
			return SweepResult{}, fmt.Errorf("扫描 work deadline: %w", err)
		}
		if t, ok := sqliteParseTS(deadline); ok && now.After(t) && CanTransition(status, WorkStalled) {
			stalled = append(stalled, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SweepResult{}, fmt.Errorf("扫描 work deadline: %w", err)
	}
	_ = rows.Close()

	type messageUpdate struct {
		id    string
		state string
	}
	var retried []messageUpdate
	rows, err = tx.QueryContext(ctx, `SELECT id, attempts, lease_until FROM work_messages
		WHERE state = ? AND lease_until <> ''`, MessageClaimed)
	if err != nil {
		return SweepResult{}, fmt.Errorf("扫描 claimed 消息: %w", err)
	}
	for rows.Next() {
		var (
			id         string
			attempts   int
			leaseUntil string
		)
		if err := rows.Scan(&id, &attempts, &leaseUntil); err != nil {
			_ = rows.Close()
			return SweepResult{}, fmt.Errorf("扫描 claimed 消息: %w", err)
		}
		t, ok := sqliteParseTS(leaseUntil)
		if !ok || !now.After(t) {
			continue
		}
		state := MessagePending
		if attempts >= s.maxTries {
			state = MessageFailed
		}
		retried = append(retried, messageUpdate{id: id, state: state})
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SweepResult{}, fmt.Errorf("扫描 claimed 消息: %w", err)
	}
	_ = rows.Close()

	var expired []string
	rows, err = tx.QueryContext(ctx, `SELECT id, created_at FROM work_messages WHERE state = ?`, MessagePending)
	if err != nil {
		return SweepResult{}, fmt.Errorf("扫描 pending 消息: %w", err)
	}
	for rows.Next() {
		var id, createdAt string
		if err := rows.Scan(&id, &createdAt); err != nil {
			_ = rows.Close()
			return SweepResult{}, fmt.Errorf("扫描 pending 消息: %w", err)
		}
		if t, ok := sqliteParseTS(createdAt); ok && now.Sub(t) > s.msgTTL {
			expired = append(expired, id)
		}
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return SweepResult{}, fmt.Errorf("扫描 pending 消息: %w", err)
	}
	_ = rows.Close()

	var res SweepResult
	updatedAt := now.UTC().Format(time.RFC3339)
	for _, id := range stalled {
		if _, err := tx.ExecContext(ctx, "UPDATE works SET status = ?, updated_at = ? WHERE id = ?",
			WorkStalled, updatedAt, id); err != nil {
			return SweepResult{}, fmt.Errorf("标记 work %s 为 stalled: %w", id, err)
		}
		res.Stalled++
	}
	for _, m := range retried {
		if _, err := tx.ExecContext(ctx, "UPDATE work_messages SET state = ?, lease_until = '' WHERE id = ?",
			m.state, m.id); err != nil {
			return SweepResult{}, fmt.Errorf("重试 message %s: %w", m.id, err)
		}
		res.Retried++
	}
	for _, id := range expired {
		if _, err := tx.ExecContext(ctx, "UPDATE work_messages SET state = ? WHERE id = ?",
			MessageExpired, id); err != nil {
			return SweepResult{}, fmt.Errorf("过期 message %s: %w", id, err)
		}
		res.Expired++
	}
	if err := tx.Commit(); err != nil {
		return SweepResult{}, err
	}
	return res, nil
}

// sqliteLoadWork 读取 work 及其 participants。
func sqliteLoadWork(ctx context.Context, q sqliteQuerier, id string) (Work, error) {
	row := q.QueryRowContext(ctx, "SELECT "+sqliteWorkColumns+" FROM works WHERE id = ?", id)
	w, err := sqliteScanWork(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Work{}, ErrWorkNotFound
	}
	if err != nil {
		return Work{}, fmt.Errorf("读取 work %s: %w", id, err)
	}
	parts, err := sqliteLoadParticipants(ctx, q, id)
	if err != nil {
		return Work{}, err
	}
	w.Participants = parts
	return w, nil
}

func sqliteScanWork(sc sqliteScanner) (Work, error) {
	var (
		w          Work
		policyJSON string
		// 剩余额度不属于 Work；GetWork 不返回它们，这里只消费掉查询列。
		toolsLeft, tokensLeft, approvalsLeft, childrenLeft, depthLeft int
	)
	err := sc.Scan(
		&w.ID, &w.Parent, &w.Root, &w.Class,
		&w.Owner.Kind, &w.Owner.ID, &w.Owner.Authority, &w.Owner.Tenant,
		&w.Agent.Kind, &w.Agent.ID, &w.Agent.Authority, &w.Agent.Tenant,
		&w.Entry.Kind, &w.Entry.ID, &w.Entry.Authority, &w.Entry.Tenant,
		&w.Goal, &w.Status, &w.Lease.DeadlineAt,
		&w.Lease.MaxToolCalls, &w.Lease.MaxTokens, &w.Lease.MaxApprovals, &w.Lease.MaxChildren, &w.Lease.MaxDepth,
		&toolsLeft, &tokensLeft, &approvalsLeft, &childrenLeft, &depthLeft,
		&policyJSON, &w.CreatedAt, &w.UpdatedAt, &w.ClosedAt,
	)
	if err != nil {
		return Work{}, err
	}
	if policyJSON != "" && policyJSON != "null" {
		if err := json.Unmarshal([]byte(policyJSON), &w.Policy); err != nil {
			return Work{}, fmt.Errorf("解析 work %s 的 policy: %w", w.ID, err)
		}
	}
	return w, nil
}

func sqliteLoadParticipants(ctx context.Context, q sqliteQuerier, workID string) ([]Participant, error) {
	rows, err := q.QueryContext(ctx, `SELECT actor_kind, actor_id, actor_authority, actor_tenant, role, status
		FROM work_participants WHERE work_id = ? ORDER BY joined_at, actor_kind, actor_id`, workID)
	if err != nil {
		return nil, fmt.Errorf("读取 work %s 的 participants: %w", workID, err)
	}
	defer func() { _ = rows.Close() }()
	var out []Participant
	for rows.Next() {
		var p Participant
		if err := rows.Scan(&p.Actor.Kind, &p.Actor.ID, &p.Actor.Authority, &p.Actor.Tenant, &p.Role, &p.Status); err != nil {
			return nil, fmt.Errorf("读取 work %s 的 participants: %w", workID, err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取 work %s 的 participants: %w", workID, err)
	}
	return out, nil
}

func sqliteUpsertParticipant(ctx context.Context, q sqliteQuerier, workID string, p Participant, now time.Time) error {
	_, err := q.ExecContext(ctx, `INSERT INTO work_participants
		(work_id, actor_kind, actor_id, actor_authority, actor_tenant, role, status, joined_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(work_id, actor_kind, actor_id) DO UPDATE SET
			actor_authority = excluded.actor_authority,
			actor_tenant = excluded.actor_tenant,
			role = excluded.role,
			status = excluded.status`,
		workID, p.Actor.Kind, p.Actor.ID, p.Actor.Authority, p.Actor.Tenant, p.Role, p.Status, sqliteTS(now))
	if err != nil {
		return fmt.Errorf("写入 work %s 的 participant %s/%s: %w", workID, p.Actor.Kind, p.Actor.ID, err)
	}
	return nil
}

func sqliteLoadRemaining(ctx context.Context, q sqliteQuerier, workID string) (Remaining, error) {
	var rem Remaining
	err := q.QueryRowContext(ctx, `SELECT deadline_at, tools_left, tokens_left, approvals_left, children_left, depth_left
		FROM works WHERE id = ?`, workID).Scan(
		&rem.DeadlineAt, &rem.ToolCalls, &rem.Tokens, &rem.Approvals, &rem.Children, &rem.Depth)
	if errors.Is(err, sql.ErrNoRows) {
		return Remaining{}, ErrWorkNotFound
	}
	if err != nil {
		return Remaining{}, fmt.Errorf("读取 work %s 的租约: %w", workID, err)
	}
	return rem, nil
}

func sqliteScanMessage(sc sqliteScanner) (QueuedMessage, error) {
	var (
		m          QueuedMessage
		body       []byte
		leaseUntil string
		createdAt  string
	)
	err := sc.Scan(
		&m.ID, &m.WorkID,
		&m.From.Kind, &m.From.ID, &m.From.Authority, &m.From.Tenant,
		&m.To.Kind, &m.To.ID, &m.To.Authority, &m.To.Tenant,
		&m.Method, &m.ReplyTo, &body, &m.State, &leaseUntil, &m.Attempts, &m.LastError, &createdAt,
	)
	if err != nil {
		return QueuedMessage{}, err
	}
	m.Body = body
	if t, ok := sqliteParseTS(leaseUntil); ok {
		m.LeaseUntil = t
	}
	t, err := time.Parse(time.RFC3339Nano, createdAt)
	if err != nil {
		return QueuedMessage{}, fmt.Errorf("解析 message %s 的 created_at %q: %w", m.ID, createdAt, err)
	}
	m.CreatedAt = t
	return m, nil
}
