// Package shared 是记忆服务的「共享经验库」：跨会话只共享提炼物。
//
// 与会话库（memory.db）是**不同的文件、不同的 *sql.DB 句柄**：
// 这里没有 messages、没有工具输出、没有文件字节，只有偏好 / 经验 / 会话摘要。
// 泄露在形状上不可能发生——不靠过滤器保证。
//
// 边界独立：只认自己的 *sql.DB，将来要跨机共享时可整块搬成独立服务
// （与会话库拆开这件事，现在由"两个库文件"承担，以后由"两个进程"承担）。
package shared

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/memnet"
)

// Store 是一个共享经验库：长期记忆 + 经验网 + 会话目录，共用一个库文件。
type Store struct {
	db       *sql.DB
	path     string
	Longterm *longterm.Store // 偏好 / 事实（用户长期记忆）
	Net      *memnet.Store   // 经验网（一句话经验 + 联想）
}

// Open 打开（或创建）共享库：建表、铺预制网。
// 与会话库一样串行化连接（SQLite 单写者；本地小数据，吞吐无虞）。
func Open(path string, now time.Time) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	if _, err := db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("建共享库表失败: %w", err)
	}
	lt, err := longterm.Open(db)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化共享长期记忆失败: %w", err)
	}
	net, err := memnet.Open(db, now)
	if err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("初始化共享经验网失败: %w", err)
	}
	return &Store{db: db, path: path, Longterm: lt, Net: net}, nil
}

// DB 返回底层句柄（迁移与测试用；业务代码请走具体方法）。
func (s *Store) DB() *sql.DB { return s.db }

// Path 是库文件路径（只读项展示用）。
func (s *Store) Path() string { return s.path }

// Close 关闭共享库。
func (s *Store) Close() error { return s.db.Close() }

const schema = `
CREATE TABLE IF NOT EXISTS shared_sessions (
	id         TEXT PRIMARY KEY,
	entry      TEXT NOT NULL DEFAULT '',
	title      TEXT NOT NULL DEFAULT '',
	private    INTEGER NOT NULL DEFAULT 0,
	updated_at TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_shared_sessions_updated ON shared_sessions(updated_at);

CREATE TABLE IF NOT EXISTS shared_tasks (
	task_id      TEXT PRIMARY KEY,
	session_id   TEXT NOT NULL,
	title        TEXT NOT NULL DEFAULT '',
	goal         TEXT NOT NULL DEFAULT '',
	outcome      TEXT NOT NULL DEFAULT '',
	capabilities TEXT NOT NULL DEFAULT '',
	created_at   TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_shared_tasks_session ON shared_tasks(session_id, created_at);

CREATE TABLE IF NOT EXISTS shared_meta (
	key   TEXT PRIMARY KEY,
	value TEXT NOT NULL DEFAULT ''
);
`

// MaxTasksPerSession 是每个会话在目录里保留的任务摘要条数（更旧的发布时顺手裁掉）。
const MaxTasksPerSession = 3

// maxTitleChars / maxGoalChars 是库里单条摘要的兜底上限：设置项只影响注入预算，
// 不能让模型偶尔把整段话塞进库。
const (
	maxTitleChars = 60
	maxGoalChars  = 240
)
