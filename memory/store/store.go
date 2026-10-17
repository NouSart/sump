// Package store 是记忆服务的会话历史存储（SQLite）。
package store

import (
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/QAQ-awa-QAQ/sump/protocol"
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
	return &Store{db: db}, nil
}

const schema = `
CREATE TABLE IF NOT EXISTS messages (
	id              INTEGER PRIMARY KEY AUTOINCREMENT,
	conversation_id TEXT NOT NULL,
	role            TEXT NOT NULL,
	content         TEXT NOT NULL,
	created_at      TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_messages_conversation ON messages(conversation_id, id);
`

// Close 关闭数据库。注意：若还有别的包共享本句柄（如 longterm），由打开方统一关闭。
func (s *Store) Close() error { return s.db.Close() }

// DB 返回底层句柄：长期记忆库（longterm）与它会话库共享**同一** 句柄——
// 同进程同库，将来跨表事务（归档/清除这类）才有原子性。谁都能调，但只有打开方负责 Close。
func (s *Store) DB() *sql.DB { return s.db }

// Append 追加一条对话消息；与该会话上一条完全相同（role+content）时跳过——
// 幂等保护（防重复交付）；代价是连续两条一模一样的消息会被合并。
func (s *Store) Append(conversationID, role, content string) error {
	var r, c string
	err := s.db.QueryRow(
		`SELECT role, content FROM messages WHERE conversation_id=? ORDER BY id DESC LIMIT 1`,
		conversationID,
	).Scan(&r, &c)
	if err == nil && r == role && c == content {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO messages(conversation_id, role, content, created_at) VALUES(?,?,?,?)`,
		conversationID, role, content, time.Now().Format(time.RFC3339Nano),
	)
	return err
}

// Recent 返回该会话最近的 limit 条消息（按时间正序）。
func (s *Store) Recent(conversationID string, limit int) ([]protocol.Message, error) {
	if limit <= 0 {
		limit = 12
	}
	rows, err := s.db.Query(
		`SELECT role, content FROM messages WHERE conversation_id=? ORDER BY id DESC LIMIT ?`,
		conversationID, limit,
	)
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

// History 返回该会话最近 limit 条消息（按时间正序，带落库时间）——供门户回填对话视图。
// 与 Recent 的区别：多带 created_at，且显式夹紧上限（界面回填不该一次拉整库）。
func (s *Store) History(conversationID string, limit int) ([]protocol.HistoryEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT role, content, created_at FROM messages WHERE conversation_id=? ORDER BY id DESC LIMIT ?`,
		conversationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]protocol.HistoryEntry, 0, limit)
	for rows.Next() {
		var m protocol.HistoryEntry
		if err := rows.Scan(&m.Role, &m.Content, &m.At); err != nil {
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
