// Package store 是记忆服务的会话历史存储（SQLite）。
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/protocol"
)

// Store 是一个会话库。
type Store struct {
	db *sql.DB

	// snapshotTotalLimit 是 context_snapshots 已存载荷（gzip 后）的总量上限，单位字节。
	// 只在 Open 时定值、之后只读：并发 SaveSnapshot 读它不需要锁（见 snapshots.go）。
	snapshotTotalLimit int64
}

// openConfig 是 Open 的启动选项（零值由 Open 补默认）。
type openConfig struct {
	snapshotTotalLimit int64
}

// Option 是 Open 的启动选项。
type Option func(*openConfig)

// WithSnapshotTotalLimit 设置已存快照总量上限（字节，按 gzip 后的 payload 累加；
// 默认 DefaultSnapshotTotalLimit = 2 GiB）。非正值让 Open 失败：容量上限没有
// 「0 = 不限」的语义，静默当成默认会掩盖配置错误。
func WithSnapshotTotalLimit(limit int64) Option {
	return func(o *openConfig) { o.snapshotTotalLimit = limit }
}

// Open 打开（或创建）数据库并建表。
//
// 版本闸门在**任何 DDL 之前**（S-001 §11.1）：
//   - 文件不存在 → 新库，建表并在同一事务里写 user_version=SchemaVersion；
//   - 存在且 user_version == SchemaVersion → 幂等的增量迁移；
//   - 存在、user_version == 0 且库里没有任何表 → 空库，按新库处理；
//   - 其余（0 但已有表 = 旧库；或高于 SchemaVersion）→ ErrSchemaVersionMismatch，
//     不建表、不写库，提示删库或改名备份后重建。
func Open(path string, opts ...Option) (*Store, error) {
	o := openConfig{snapshotTotalLimit: DefaultSnapshotTotalLimit}
	for _, opt := range opts {
		if opt != nil {
			opt(&o)
		}
	}
	if o.snapshotTotalLimit <= 0 {
		return nil, fmt.Errorf("快照总量上限必须是正数，收到 %d 字节", o.snapshotTotalLimit)
	}
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("创建会话库目录 %s: %w", dir, err)
		}
	}

	// 预检只读：先判断库状态，再决定允不允许写。
	state, err := inspectSchema(path)
	if err != nil {
		return nil, err
	}
	if !allowWrite(state) {
		return nil, &SchemaVersionMismatchError{Path: path, Current: state.version, Expected: SchemaVersion}
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("打开会话库 %s: %w", path, err)
	}
	// SQLite 单写者：串行化连接，避免并发写锁冲突（本地小数据，吞吐无虞）。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("设置会话库 %s 的 busy_timeout: %w", path, err)
	}
	// 建表与 user_version 在同一事务：中途失败不会留下「建了表但没版本号」的库。
	if err := applySchema(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化会话库 %s: %w", path, err)
	}

	if err := migrate(db); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("迁移会话库 %s: %w", path, err)
	}
	return &Store{db: db, snapshotTotalLimit: o.snapshotTotalLimit}, nil
}

// migrate 补上后来加的列 / 索引（幂等：已有就跳过），并回填老数据。
// 它只对「版本已匹配」的库执行——版本闸门在 Open 里、在任何 DDL 之前（S-001 §11.1）。
func migrate(db *sql.DB) error {
	// 先补 messages 的历史列（kind/meta/seq/task_id）。
	have, err := tableColumns(db, "messages")
	if err != nil {
		return fmt.Errorf("读取 messages 表结构: %w", err)
	}
	for _, m := range migrations {
		if have[m.name] {
			continue
		}
		if _, err := db.Exec(m.ddl); err != nil {
			return fmt.Errorf("为 messages 补列 %s: %w", m.name, err)
		}
	}
	// v2 新列（S-001 §4.1 / §4.2）。放在建索引之前：老库要先有列才能建索引。
	for _, m := range v2ColumnMigrations {
		if err := addColumnIfMissing(db, m.table, m.name, m.ddl); err != nil {
			return err
		}
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_messages_work ON messages(work_id, id)`); err != nil {
		return fmt.Errorf("建 messages(work_id, id) 索引: %w", err)
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
CREATE TABLE IF NOT EXISTS memory_staging (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL DEFAULT '',
	role            TEXT NOT NULL DEFAULT 'user',
	content         TEXT NOT NULL,
	status          TEXT NOT NULL DEFAULT 'pending',
	attempts        INTEGER NOT NULL DEFAULT 0,
	last_error      TEXT NOT NULL DEFAULT '',
	created_at      TEXT NOT NULL,
	updated_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_memory_staging_pending ON memory_staging(status, id);
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

// v2ColumnMigrations 是给既有表补的列（S-001 §4.1 / §4.2），幂等：已有就跳过。
//   - works.task：call 账本解析 work → task 联表用（calls 表本身没有 task 列）；
//   - messages.work_id / visibility：v2 落库的归属与可见性标签。
//
// report_receipts 的 phase / final / data 在 v2Schema 的建表语句里，不走补列。
var v2ColumnMigrations = []struct{ table, name, ddl string }{
	{"works", "task", `ALTER TABLE works ADD COLUMN task TEXT NOT NULL DEFAULT ''`},
	{"messages", "work_id", `ALTER TABLE messages ADD COLUMN work_id TEXT NOT NULL DEFAULT ''`},
	{"messages", "visibility", `ALTER TABLE messages ADD COLUMN visibility TEXT NOT NULL DEFAULT 'work'`},
}

// tableColumns 返回一张表已有的列名集合（PRAGMA table_info）。
func tableColumns(db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
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
			return nil, err
		}
		have[name] = true
	}
	return have, rows.Err()
}

// addColumnIfMissing 用 PRAGMA table_info 判断列是否存在，缺了才 ALTER（幂等）。
func addColumnIfMissing(db *sql.DB, table, column, ddl string) error {
	have, err := tableColumns(db, table)
	if err != nil {
		return fmt.Errorf("读取表 %s 结构: %w", table, err)
	}
	if have[column] {
		return nil
	}
	if _, err := db.Exec(ddl); err != nil {
		return fmt.Errorf("为表 %s 补列 %s: %w", table, column, err)
	}
	return nil
}

// Close 关闭数据库。注意：句柄交给过别人（DB()）也仍由打开方统一关闭。
func (s *Store) Close() error { return s.db.Close() }

// DB 返回底层句柄。生产里只有一个消费者：main 的一次性旧库迁移（shared.MigrateFrom(st.DB())）；
// 测试也拿它直接建表。共享经验库**不是**从这个句柄接过去的——它是另一个文件、另一个句柄，
// 两个库的分工见 shared/db.go 的包注释。谁都能调，但只有打开方负责 Close。
func (s *Store) DB() *sql.DB { return s.db }

// Record 追加一条会话记录。Kind 为空时按 Role 推断（老调用方不用改）。
//
// 幂等保护只对**对话消息**（user/assistant）生效：上一行 role+content 完全相同就跳过
// （防重复交付）。事件类记录（reasoning/tool_call/tool_result）不去重——连跑两条一样的命令
// 是两件事，吞掉才是错的。
//
// Seq 由写入方给（会话内单调）：并发写（Fire 是 goroutine）也能按真实先后排序；
// 为 0 时退化为按落库顺序（老数据 / 不关心顺序的调用方）。
//
// **归档不设门槛**：这里不查 conversations.status——归档 = 移出默认列表，不是只读。
// 改成拒写不是加个 if 那么简单：调用方（reasoner / 各入口）不看会话状态、多数还用 Fire
// （发出即完，失败只进日志），门户又没有「取消归档」入口，拦下来就是悄悄丢历史。
// 依据与逐条证据钉在 tests/conversations_test.go 的 TestConversationTable。
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
