// 任务：一段有名字、有始有终的工作。会话记录挂在任务下，agent 在任务里工作。
//
// 这里只存事实（名字 / 状态 / 归属），不解释语义：完成与否由 reasoner 判定后写进来。
// 同一个会话可以有多个任务，但**最多一个进行中**——"当前任务"= 最近活跃的那个 active。
package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// 任务状态。
const (
	TaskActive = "active"
	TaskDone   = "done"
)

// newTaskID 生成一个任务 id（时间 + 随机，够用且可排序）。
func newTaskID(now time.Time) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("t%s-%s", now.Format("20060102T150405"), hex.EncodeToString(b[:]))
}

// CurrentTask 返回会话里最近活跃的进行中任务；没有就 ok=false。
func (s *Store) CurrentTask(conversationID string) (protocol.Task, bool, error) {
	var t protocol.Task
	err := s.db.QueryRow(
		`SELECT id, conversation_id, name, status, created_at, updated_at FROM tasks
		 WHERE conversation_id=? AND status=? ORDER BY updated_at DESC LIMIT 1`,
		conversationID, TaskActive,
	).Scan(&t.ID, &t.ConversationID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Task{}, false, nil
	}
	if err != nil {
		return protocol.Task{}, false, err
	}
	return t, true, nil
}

// LatestTask 返回会话里最近活跃的任务（不论状态）——门户在"没有进行中任务"时看它。
func (s *Store) LatestTask(conversationID string) (protocol.Task, bool, error) {
	var t protocol.Task
	err := s.db.QueryRow(
		`SELECT id, conversation_id, name, status, created_at, updated_at FROM tasks
		 WHERE conversation_id=? ORDER BY updated_at DESC LIMIT 1`,
		conversationID,
	).Scan(&t.ID, &t.ConversationID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return protocol.Task{}, false, nil
	}
	if err != nil {
		return protocol.Task{}, false, err
	}
	return t, true, nil
}

// OpenTask 新建一个进行中的任务。
func (s *Store) OpenTask(conversationID, name string) (protocol.Task, error) {
	now := time.Now()
	at := now.Format(time.RFC3339Nano)
	if name == "" {
		name = "未命名任务"
	}
	t := protocol.Task{
		ID: newTaskID(now), ConversationID: conversationID, Name: name,
		Status: TaskActive, CreatedAt: at, UpdatedAt: at,
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks(id, conversation_id, name, status, created_at, updated_at) VALUES(?,?,?,?,?,?)`,
		t.ID, t.ConversationID, t.Name, t.Status, t.CreatedAt, t.UpdatedAt,
	); err != nil {
		return protocol.Task{}, err
	}
	return t, nil
}

// EnsureTask 返回当前进行中的任务；没有就新建一个（名字由调用方给）。
// 这是"agent 必须在任务中工作"的落地：拿不到任务就不该往下走。
//
// 并发安全靠数据库的部分唯一索引：两个调用同时发现"没有任务"时，只有一个能建上，
// 另一个会撞唯一约束 → 回头读那个刚建好的（任务不会分叉）。
func (s *Store) EnsureTask(conversationID, fallbackName string) (protocol.Task, error) {
	if t, ok, err := s.CurrentTask(conversationID); err != nil {
		return protocol.Task{}, err
	} else if ok {
		return t, nil
	}
	t, err := s.OpenTask(conversationID, fallbackName)
	if err != nil {
		if existing, ok, err2 := s.CurrentTask(conversationID); err2 == nil && ok {
			return existing, nil
		}
		return protocol.Task{}, err
	}
	return t, nil
}

// FinishTask 完结任务（可顺带改名）。
func (s *Store) FinishTask(taskID, name string) (protocol.Task, error) {
	now := time.Now().Format(time.RFC3339Nano)
	if name != "" {
		if _, err := s.db.Exec(`UPDATE tasks SET status=?, name=?, updated_at=? WHERE id=?`, TaskDone, name, now, taskID); err != nil {
			return protocol.Task{}, err
		}
	} else {
		if _, err := s.db.Exec(`UPDATE tasks SET status=?, updated_at=? WHERE id=?`, TaskDone, now, taskID); err != nil {
			return protocol.Task{}, err
		}
	}
	return s.TaskByID(taskID)
}

// ResumeTask 把任务顶成最近活跃的那个（门户"恢复未完结任务"）。
func (s *Store) ResumeTask(taskID string) (protocol.Task, error) {
	now := time.Now().Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`UPDATE tasks SET updated_at=? WHERE id=?`, now, taskID); err != nil {
		return protocol.Task{}, err
	}
	return s.TaskByID(taskID)
}

// RenameTask 改任务名（不改状态）。
func (s *Store) RenameTask(taskID, name string) (protocol.Task, error) {
	if name == "" {
		return s.TaskByID(taskID)
	}
	if _, err := s.db.Exec(`UPDATE tasks SET name=?, updated_at=? WHERE id=?`, name, time.Now().Format(time.RFC3339Nano), taskID); err != nil {
		return protocol.Task{}, err
	}
	return s.TaskByID(taskID)
}

// TouchTask 刷新任务的活跃时间（每写入一条记录时调用，保证"最近活跃"排序是真的）。
func (s *Store) TouchTask(taskID string) error {
	if taskID == "" {
		return nil
	}
	_, err := s.db.Exec(`UPDATE tasks SET updated_at=? WHERE id=?`, time.Now().Format(time.RFC3339Nano), taskID)
	return err
}

// TaskByID 按 id 取任务。
func (s *Store) TaskByID(taskID string) (protocol.Task, error) {
	var t protocol.Task
	err := s.db.QueryRow(
		`SELECT id, conversation_id, name, status, created_at, updated_at FROM tasks WHERE id=?`,
		taskID,
	).Scan(&t.ID, &t.ConversationID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt)
	if err != nil {
		return protocol.Task{}, err
	}
	return t, nil
}

// ListTasks 列出会话的任务（最近活跃在前）。
func (s *Store) ListTasks(conversationID string, limit int) ([]protocol.Task, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.db.Query(
		`SELECT id, conversation_id, name, status, created_at, updated_at FROM tasks
		 WHERE conversation_id=? ORDER BY updated_at DESC LIMIT ?`,
		conversationID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]protocol.Task, 0, limit)
	for rows.Next() {
		var t protocol.Task
		if err := rows.Scan(&t.ID, &t.ConversationID, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
