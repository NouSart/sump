package store

// 会话表：把"会话"从散落在 messages/tasks 里的一个字符串，变成有身份的一行。
//
// 为什么要这张表：没有它，历史记录页只能靠 `GROUP BY conversation_id` 现凑一个列表——
// 起不了名、分不出入口、收不进归档，多会话也就无从谈起。它**只存元信息**：
// 消息数、任务数、最后一条说什么，全部现算（冗余字段迟早和真相打架）。

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

const (
	// ConversationActive 是正常可用的会话；ConversationArchived 是收起来的（只读）。
	ConversationActive   = "active"
	ConversationArchived = "archived"

	// conversationTouchInterval 是 updated_at 的最小刷新间隔：每条消息都写一次 updated_at
	// 会让热路径多一次写（同一毫秒内尤其浪费）；差这么点时间，列表排序看不出区别。
	conversationTouchInterval = 30 * time.Second
)

// EnsureConversation 保证会话存在（幂等），并记下它的入口。
//
// 这是"建会话"的唯一入口：消息落库、任务开启都会先过这里，所以列表里不会出现
// 有消息却没有会话的孤儿。title 非空且当前为空时填上（不覆盖已有的名字——
// 起名是件明确的事，不能靠"顺带"）。
func (s *Store) EnsureConversation(id, title, entry string) error {
	if id == "" {
		return errors.New("会话 id 不能为空")
	}
	now := time.Now().Format(time.RFC3339Nano)
	_, err := s.db.Exec(
		`INSERT INTO conversations(id, title, entry, status, created_at, updated_at)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET
		   title = CASE WHEN conversations.title = '' THEN excluded.title ELSE conversations.title END,
		   entry = CASE WHEN conversations.entry = '' THEN excluded.entry ELSE conversations.entry END`,
		id, title, entry, ConversationActive, now, now,
	)
	return err
}

// touchConversation 把会话的 updated_at 推到此刻（不存在就先建）。
// 由 Record 在**真正写入一条记录之后**调用：只有落库成功才算"活跃"。
func (s *Store) touchConversation(id string) error {
	now := time.Now()
	// 先看它是不是刚活跃过（省一次写）；查不到才 insert，查到且够旧才 update。
	var last string
	err := s.db.QueryRow(`SELECT updated_at FROM conversations WHERE id=?`, id).Scan(&last)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		_, err = s.db.Exec(
			`INSERT INTO conversations(id, title, entry, status, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
			id, "", "", ConversationActive, now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano),
		)
		return err
	case err != nil:
		return err
	}
	if t, perr := time.Parse(time.RFC3339Nano, last); perr == nil && now.Sub(t) < conversationTouchInterval {
		return nil
	}
	_, err = s.db.Exec(`UPDATE conversations SET updated_at=? WHERE id=?`, now.Format(time.RFC3339Nano), id)
	return err
}

// ListConversations 列出会话（按最近活跃排序）。零会话时返回空切片，不是 nil。
func (s *Store) ListConversations(limit int, includeArchived bool) ([]protocol.Conversation, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	q := `SELECT id, title, entry, status, created_at, updated_at FROM conversations`
	if !includeArchived {
		q += ` WHERE status = 'active'`
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	rows, err := s.db.Query(q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []protocol.Conversation{}
	for rows.Next() {
		var c protocol.Conversation
		if err := rows.Scan(&c.ID, &c.Title, &c.Entry, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ConversationByID 取一个会话（不存在时报错——调用方给错了 id，别悄悄造一个）。
func (s *Store) ConversationByID(id string) (protocol.Conversation, error) {
	var c protocol.Conversation
	err := s.db.QueryRow(
		`SELECT id, title, entry, status, created_at, updated_at FROM conversations WHERE id=?`, id,
	).Scan(&c.ID, &c.Title, &c.Entry, &c.Status, &c.CreatedAt, &c.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return c, fmt.Errorf("会话 %q 不存在", id)
	}
	return c, err
}

// RenameConversation 改会话名（空串 = 清掉名字）。会话不存在时报错。
func (s *Store) RenameConversation(id, title string) (protocol.Conversation, error) {
	res, err := s.db.Exec(`UPDATE conversations SET title=? WHERE id=?`, title, id)
	if err != nil {
		return protocol.Conversation{}, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return protocol.Conversation{}, fmt.Errorf("会话 %q 不存在", id)
	}
	return s.ConversationByID(id)
}
