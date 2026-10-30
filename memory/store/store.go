// Package store 是记忆服务的会话历史存储（SQLite）。
package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/protocol"
)

// Store 是一个会话库。
type Store struct {
	db *sql.DB
}

// Open 打开（或创建）数据库并建表。
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite 单写者：串行化连接，避免并发写锁冲突（本地小数据，吞吐无虞）。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, err
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

// migrate 补上后来加的列（幂等：已有就跳过）。
func migrate(db *sql.DB) error {
	rows, err := db.Query(`PRAGMA table_info(messages)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid     int
			name    string
			ctype   string
			notNull int
			dflt    any
			pk      int
		)
		if err := rows.Scan(&cid, &name, &ctype, &notNull, &dflt, &pk); err != nil {
			_ = rows.Close()
			return err
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return err
	}
	_ = rows.Close()
	for _, m := range migrations {
		if have[m.name] {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil {
			return err
		}
	}
	// 老数据没有序号：用自增主键回填（同库内单调，跨会话比较无意义但排序仍正确）。
	if _, err := db.Exec(`UPDATE messages SET seq = id WHERE seq = 0`); err != nil {
		return err
	}
	// 老数据没有任务：每个会话补一个已完结的「历史记录」任务，把旧消息挂上去（不丢历史）。
	if _, err := db.Exec(`INSERT OR IGNORE INTO tasks(id, conversation_id, name, status, created_at, updated_at)
		SELECT 'legacy-' || conversation_id, conversation_id, '历史记录', 'done', MIN(created_at), MAX(created_at)
		FROM messages WHERE task_id = '' GROUP BY conversation_id`); err != nil {
		return err
	}
	if _, err := db.Exec(`UPDATE messages SET task_id = 'legacy-' || conversation_id WHERE task_id = ''`); err != nil {
		return err
	}
	// 先收拾历史遗留的多余 active 任务（只保留最近活跃的那个），再上唯一索引。
	if _, err := db.Exec(`UPDATE tasks SET status = 'done'
		WHERE status = 'active' AND id NOT IN (
			SELECT t.id FROM tasks t
			WHERE t.conversation_id = tasks.conversation_id AND t.status = 'active'
			ORDER BY t.updated_at DESC LIMIT 1
		)`); err != nil {
		return err
	}
	if _, err := db.Exec(activeTaskIndex); err != nil {
		return err
	}
	// 老数据没有会话表：从 messages / tasks 里已有的 conversation_id 回填，
	// 保证升级后历史记录页立刻能看到原来的会话（顺序按各会话的首末时间）。
	if _, err := db.Exec(`INSERT OR IGNORE INTO conversations(id, title, entry, status, created_at, updated_at)
		SELECT conversation_id, '', '', 'active', MIN(created_at), MAX(created_at)
		FROM messages GROUP BY conversation_id`); err != nil {
		return err
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO conversations(id, title, entry, status, created_at, updated_at)
		SELECT conversation_id, '', '', 'active', MIN(created_at), MAX(updated_at)
		FROM tasks GROUP BY conversation_id`); err != nil {
		return err
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS conversations (
	id              TEXT PRIMARY KEY,
	title           TEXT NOT NULL DEFAULT '',
	entry           TEXT NOT NULL DEFAULT '',
	status          TEXT NOT NULL DEFAULT 'active',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_conversations_updated ON conversations(updated_at);
CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL,
	role            TEXT NOT NULL,
	content         TEXT NOT NULL,
	created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, id);
CREATE TABLE IF NOT EXISTS tasks (
	id              TEXT PRIMARY KEY,
	conversation_id TEXT NOT NULL,
	name            TEXT NOT NULL,
	status          TEXT NOT NULL,
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tasks_conversation ON tasks(conversation_id, updated_at);
`

// activeTaskIndex 保证"一个会话最多一个进行中的任务"。
// 没有它，同一时刻的两条路径（落消息 / 组装上下文）会各建一个——任务就分叉了。
// 用部分唯一索引把这条规则交给数据库，代码侧只需在建不上时回头读现有的那个。
const activeTaskIndex = `CREATE UNIQUE INDEX IF NOT EXISTS idx_tasks_one_active
	ON tasks(conversation_id) WHERE status = 'active'`

// migrations 是建表之后的补列（老库平滑升级：只加列，不动数据）。
//   - kind：记录类型（user/assistant/reasoning/tool_call/tool_result；老数据为空 = 按 role 推断）
//   - meta：给人看的附加信息（JSON：工具名、命令、call_id、是否截断…）
//   - seq：会话内单调序号——写入方（reasoner）给，保证并发写也能按真实先后排序
//   - task_id：这条记录属于哪个任务
var migrations = []struct{ name, ddl string }{
	{"kind", `ALTER TABLE messages ADD COLUMN kind TEXT NOT NULL DEFAULT ''`},
	{"meta", `ALTER TABLE messages ADD COLUMN meta TEXT NOT NULL DEFAULT ''`},
	{"seq", `ALTER TABLE messages ADD COLUMN seq INTEGER NOT NULL DEFAULT 0`},
	{"task_id", `ALTER TABLE messages ADD COLUMN task_id TEXT NOT NULL DEFAULT ''`},
}

// Close 关闭数据库。注意：若还有别的包共享本句柄（如 longterm），由打开方统一关闭。
func (s *Store) Close() error { return s.db.Close() }

// DB 返回底层句柄：长期记忆库（longterm）与它会话库共享**同一** 句柄——
// 同进程同库，将来跨表事务（归档/清除这类）才有原子性。谁都能调，但只有打开方负责 Close。
func (s *Store) DB() *sql.DB { return s.db }

// Record 追加一条会话记录。Kind 为空时按 Role 推断（老调用方不用改）。
//
// 幂等保护只对**对话消息**（user/assistant）生效：上一行 role+content 完全相同就跳过
// （防重复交付）。事件类记录（reasoning/tool_call/tool_result）不去重——连跑两条一样的命令
// 是两件事，吞掉才是错的。
//
// Seq 由写入方给（会话内单调）：并发写（Fire 是 goroutine）也能按真实先后排序；
// 为 0 时退化为按落库顺序（老数据 / 不关心顺序的调用方）。
func (s *Store) Record(conversationID, role, content, kind, meta string, seq int64, taskID string) error {
	if kind == "" {
		kind = kindOfRole(role)
	}
	if kind == "user" || kind == "assistant" {
		var r, c, k string
		err := s.db.QueryRow(
			`SELECT role, content, kind FROM messages
			 WHERE conversation_id=? AND kind IN ('', 'user', 'assistant')
			 ORDER BY seq DESC, id DESC LIMIT 1`,
			conversationID,
		).Scan(&r, &c, &k)
		if err == nil && r == role && c == content && (k == kind || k == "") {
			return nil
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
	}
	// seq 为 0 时按会话内最大值 +1 补齐（老调用方不关心顺序也不会踩到 seq=0）。
	_, err := s.db.Exec(
		`INSERT INTO messages(conversation_id, role, content, kind, meta, seq, created_at, task_id)
		 VALUES(?,?,?,?,?, COALESCE(NULLIF(?, 0), (SELECT COALESCE(MAX(seq),0)+1 FROM messages WHERE conversation_id=?)), ?, ?)`,
		conversationID, role, content, kind, meta, seq, conversationID, time.Now().Format(time.RFC3339Nano), taskID,
	)
	if err != nil {
		return err
	}
	// 会话的活跃时间跟着真正的写入走：写失败不推（否则列表里会有个"刚活跃过"的空会话）。
	return s.touchConversation(conversationID)
}

// Append 是 Record 的简写（对话消息，不关心顺序与任务）：保留给老调用方与测试。
func (s *Store) Append(conversationID, role, content string) error {
	return s.Record(conversationID, role, content, "", "", 0, "")
}

// kindOfRole 按 role 推断记录类型（老数据没有 kind 列时的兜底）。
func kindOfRole(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	default:
		return "assistant"
	}
}

// Recent 返回该会话最近的 limit 条**对话**消息（按时间正序）。
// 只取 user/assistant：思考与工具轨迹是给人看的，不进 LLM 上下文
// （reasoning 回灌纯浪费，工具结果缺少配对的 tool_calls 会被上游拒）。
// taskID 非空时只取该任务的消息——agent 在任务里工作，不跨任务混上下文。
func (s *Store) Recent(conversationID string, limit int, taskID string) ([]protocol.Message, error) {
	if limit <= 0 {
		limit = 12
	}
	query := `SELECT role, content FROM messages
		 WHERE conversation_id=? AND kind IN ('', 'user', 'assistant')`
	args := []any{conversationID}
	if taskID != "" {
		query += ` AND task_id=?`
		args = append(args, taskID)
	}
	query += ` ORDER BY seq DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]protocol.Message, 0, limit)
	for rows.Next() {
		var m protocol.Message
		if err := rows.Scan(&m.Role, &m.Content); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 倒序 → 正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}

// History 返回该会话最近 limit 条记录（按时间正序，带落库时间与序号）——供门户回填对话视图。
// 与 Recent 的区别：包含思考与工具轨迹（给人看），且显式夹紧上限。
// AfterSeq > 0 时只取更新的记录（增量拉取，界面不必每次全量重来）。
func (s *Store) History(conversationID string, limit int, afterSeq int64, taskID string) ([]protocol.HistoryEntry, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		limit = 500
	}
	query := `SELECT id, seq, role, kind, content, meta, created_at, task_id FROM messages
		 WHERE conversation_id=? AND seq > ?`
	args := []any{conversationID, afterSeq}
	if taskID != "" {
		query += ` AND task_id=?`
		args = append(args, taskID)
	}
	query += ` ORDER BY seq DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]protocol.HistoryEntry, 0, limit)
	for rows.Next() {
		var m protocol.HistoryEntry
		if err := rows.Scan(&m.ID, &m.Seq, &m.Role, &m.Kind, &m.Content, &m.Meta, &m.At, &m.TaskID); err != nil {
			return nil, err
		}
		if m.Kind == "" {
			m.Kind = kindOfRole(m.Role)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 倒序 → 正序
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, nil
}
