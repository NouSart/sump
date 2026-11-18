package store

// 记忆整理池：把「原始观察」留在私有会话库，等 reasoner 分类后再决定是否
// 归档到共享长期记忆 / 经验网。
//
// 这里是**私有数据**：原文只在本库出现；归档前必须经过 shared.Sanitize。
// 池子只是待处理队列，可重试、可丢弃，不承担权威数据职责。

import (
	"errors"
	"strings"
	"time"
)

const (
	// StagingMaxAttempts 是单条候选的模型失败重试上限：超过就标 failed，
	// 避免一条毒数据每次整理都卡住整批。
	StagingMaxAttempts = 3
	// StagingDefaultBatch 是每次取出的候选条数（控制提示词与模型成本）。
	StagingDefaultBatch = 20
)

// StagedItem 是一条待整理的原始观察。
type StagedItem struct {
	ID             int64
	ConversationID string
	Role           string
	Content        string
	Status         string
	Attempts       int
	LastError      string
	CreatedAt      string
}

// Stage 把一条原始观察放进整理池（只存私有库；空内容拒绝）。
func (s *Store) Stage(conversationID, role, content string) (int64, error) {
	role = strings.TrimSpace(role)
	if role == "" {
		role = "user"
	}
	content = strings.TrimSpace(content)
	if content == "" {
		return 0, errors.New("stage: content 必填")
	}
	now := time.Now().Format(time.RFC3339Nano)
	res, err := s.db.Exec(
		`INSERT INTO memory_staging(conversation_id, role, content, status, attempts, last_error, created_at, updated_at)
		 VALUES(?,?,?,'pending',0,'',?,?)`,
		conversationID, role, content, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// PendingStaged 取待处理候选（按 id 升序；limit <= 0 用默认批大小）。
func (s *Store) PendingStaged(limit int) ([]StagedItem, error) {
	if limit <= 0 {
		limit = StagingDefaultBatch
	}
	rows, err := s.db.Query(
		`SELECT id, conversation_id, role, content, status, attempts, last_error, created_at
		 FROM memory_staging WHERE status='pending' ORDER BY id LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StagedItem
	for rows.Next() {
		var it StagedItem
		if err := rows.Scan(&it.ID, &it.ConversationID, &it.Role, &it.Content, &it.Status, &it.Attempts, &it.LastError, &it.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// MarkStaged 结束一条候选的处理（done / skipped / failed）。
func (s *Store) MarkStaged(id int64, status, lastErr string) error {
	switch status {
	case "done", "skipped", "failed":
	default:
		return errors.New("mark staged: 非法状态 " + status)
	}
	_, err := s.db.Exec(
		`UPDATE memory_staging SET status=?, last_error=?, updated_at=? WHERE id=?`,
		status, lastErr, time.Now().Format(time.RFC3339Nano), id,
	)
	return err
}

// RetryStaged 记一次失败：未到上限则留在 pending，到了就标 failed。
func (s *Store) RetryStaged(id int64, lastErr string) error {
	_, err := s.db.Exec(
		`UPDATE memory_staging
		 SET attempts=attempts+1,
		     status=CASE WHEN attempts+1 >= ? THEN 'failed' ELSE 'pending' END,
		     last_error=?, updated_at=?
		 WHERE id=?`,
		StagingMaxAttempts, lastErr, time.Now().Format(time.RFC3339Nano), id,
	)
	return err
}

// StagingCounts 返回各状态条数（测试 / 运维观测）。
func (s *Store) StagingCounts() (pending, done, skipped, failed int, err error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM memory_staging GROUP BY status`)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return 0, 0, 0, 0, err
		}
		switch status {
		case "pending":
			pending = n
		case "done":
			done = n
		case "skipped":
			skipped = n
		case "failed":
			failed = n
		}
	}
	if err := rows.Err(); err != nil {
		return 0, 0, 0, 0, err
	}
	return pending, done, skipped, failed, nil
}
