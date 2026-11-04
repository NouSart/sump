package shared

// 会话目录：回答"别的会话在干什么"。
//
// 每个会话一行 + 最近 MaxTasksPerSession 条任务摘要。条目全是提炼物：
// 一句在做什么、一句做到哪了、用过的能力族。**没有原文**。
//
// 会话 id 只用于归并与排除自己，界面上展示的是入口 + 标题（QQ 号之类的标识不外露）。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// PublishInput 是一条待发布的摘要（已由调用方从模型输出里取好）。
type PublishInput struct {
	SessionID    string
	SessionTitle string
	TaskID       string
	TaskTitle    string
	Goal         string
	Outcome      string
	Capabilities []string
	Private      bool
}

// Task 是目录里的一条任务摘要。
type Task struct {
	TaskID       string
	SessionID    string
	Title        string
	Goal         string
	Outcome      string
	Capabilities []string
	CreatedAt    time.Time
}

// Session 是目录里的一条会话。
type Session struct {
	ID        string
	Entry     string
	Title     string
	UpdatedAt time.Time
	Tasks     []Task
}

// Publish 写一条任务摘要（幂等：同一个 task_id 再发一次就是覆盖）。
//
// private = 这个任务不适合进目录：**什么都不写**（连会话行也不碰），
// 已经发布过的旧摘要也不删——是否撤回由调用方显式 Forget 决定。
func (s *Store) Publish(now time.Time, in PublishInput) (bool, error) {
	sessionID := strings.TrimSpace(in.SessionID)
	taskID := strings.TrimSpace(in.TaskID)
	if sessionID == "" {
		return false, errors.New("publish_session: conversation_id 必填")
	}
	if taskID == "" {
		return false, errors.New("publish_session: task_id 必填")
	}
	if in.Private {
		return false, nil
	}

	title := Sanitize(in.TaskTitle, maxTitleChars)
	goal := Sanitize(in.Goal, maxGoalChars)
	outcome := Sanitize(in.Outcome, maxGoalChars)
	if title == "" && goal == "" && outcome == "" {
		// 什么都没提炼出来：不占目录位置（"我完成了任务"这类空话不进库）。
		return false, nil
	}
	caps := SanitizeCapabilities(in.Capabilities)
	capsJSON := encodeCaps(caps)
	at := now.Format(time.RFC3339Nano)

	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	// 会话名只在为空时补：起名是件明确的事，不能被"顺带"覆盖。
	sessionTitle := Sanitize(in.SessionTitle, maxTitleChars)
	if _, err := tx.Exec(
		`INSERT INTO shared_sessions(id, entry, title, private, updated_at) VALUES(?,?,?,0,?)
		 ON CONFLICT(id) DO UPDATE SET
		   entry      = CASE WHEN shared_sessions.entry = '' THEN excluded.entry ELSE shared_sessions.entry END,
		   title      = CASE WHEN shared_sessions.title = '' THEN excluded.title ELSE shared_sessions.title END,
		   private    = 0,
		   updated_at = excluded.updated_at`,
		sessionID, EntryOf(sessionID), sessionTitle, at,
	); err != nil {
		return false, err
	}

	if _, err := tx.Exec(
		`INSERT INTO shared_tasks(task_id, session_id, title, goal, outcome, capabilities, created_at)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(task_id) DO UPDATE SET
		   session_id   = excluded.session_id,
		   title        = excluded.title,
		   goal         = excluded.goal,
		   outcome      = excluded.outcome,
		   capabilities = excluded.capabilities`,
		taskID, sessionID, title, goal, outcome, capsJSON, at,
	); err != nil {
		return false, err
	}

	// 只留最近 N 条：目录是给人看的，不是归档。
	if limit := MaxTasksPerSession; limit > 0 {
		if _, err := tx.Exec(
			`DELETE FROM shared_tasks WHERE session_id = ? AND task_id NOT IN (
			   SELECT task_id FROM shared_tasks WHERE session_id = ? ORDER BY created_at DESC, task_id DESC LIMIT ?
			 )`,
			sessionID, sessionID, limit,
		); err != nil {
			return false, err
		}
	}
	return true, tx.Commit()
}

// List 取会话目录：最近活跃在前，排除 exclude（通常是调用方自己），可选的 query 只做子串过滤。
func (s *Store) List(limit int, exclude, query string) ([]Session, error) {
	if limit <= 0 {
		limit = 5
	}
	if limit > 50 {
		limit = 50
	}
	q := `SELECT id, entry, title, updated_at FROM shared_sessions WHERE private = 0`
	args := []any{}
	if strings.TrimSpace(exclude) != "" {
		q += ` AND id <> ?`
		args = append(args, exclude)
	}
	if like := strings.TrimSpace(query); like != "" {
		q += ` AND id IN (SELECT session_id FROM shared_tasks WHERE title LIKE ? OR goal LIKE ? OR outcome LIKE ?)`
		args = append(args, "%"+like+"%", "%"+like+"%", "%"+like+"%")
	}
	q += ` ORDER BY updated_at DESC LIMIT ?`
	args = append(args, limit)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{}
	for rows.Next() {
		var (
			sess Session
			at   string
		)
		if err := rows.Scan(&sess.ID, &sess.Entry, &sess.Title, &at); err != nil {
			return nil, err
		}
		sess.UpdatedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, sess)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		tasks, err := s.tasksOf(out[i].ID, MaxTasksPerSession)
		if err != nil {
			return nil, err
		}
		out[i].Tasks = tasks
	}
	return out, nil
}

func (s *Store) tasksOf(sessionID string, limit int) ([]Task, error) {
	if limit <= 0 {
		limit = MaxTasksPerSession
	}
	rows, err := s.db.Query(
		`SELECT task_id, session_id, title, goal, outcome, capabilities, created_at
		   FROM shared_tasks WHERE session_id = ? ORDER BY created_at DESC, task_id DESC LIMIT ?`,
		sessionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Task{}
	for rows.Next() {
		var (
			t    Task
			caps string
			at   string
		)
		if err := rows.Scan(&t.TaskID, &t.SessionID, &t.Title, &t.Goal, &t.Outcome, &caps, &at); err != nil {
			return nil, err
		}
		t.Capabilities = decodeCaps(caps)
		t.CreatedAt, _ = time.Parse(time.RFC3339Nano, at)
		out = append(out, t)
	}
	return out, rows.Err()
}

// ForgetResult 是删除计数。
type ForgetResult struct {
	Sessions int
	Tasks    int
}

// Forget 删共享物：给 TaskID 只删那一条（删空则该会话也不再列出）；给 SessionID 删整个会话。
func (s *Store) Forget(sessionID, taskID string) (ForgetResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	taskID = strings.TrimSpace(taskID)
	var res ForgetResult
	switch {
	case taskID != "":
		tx, err := s.db.Begin()
		if err != nil {
			return res, err
		}
		defer func() { _ = tx.Rollback() }()
		// 先记下它属于哪个会话：删完就查不到了。
		var sid string
		if err := tx.QueryRow(`SELECT session_id FROM shared_tasks WHERE task_id = ?`, taskID).Scan(&sid); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return res, tx.Commit() // 没有这条：不是错误（重复删除 / 服务重启过）
			}
			return res, err
		}
		r, err := tx.Exec(`DELETE FROM shared_tasks WHERE task_id = ?`, taskID)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Tasks = int(n)
		var left int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM shared_tasks WHERE session_id = ?`, sid).Scan(&left); err != nil {
			return res, err
		}
		// 会话下已经没有摘要了：这一行不再有意义。
		if left == 0 {
			r, err = tx.Exec(`DELETE FROM shared_sessions WHERE id = ?`, sid)
			if err != nil {
				return res, err
			}
			n, _ = r.RowsAffected()
			res.Sessions = int(n)
		}
		return res, tx.Commit()
	case sessionID != "":
		tx, err := s.db.Begin()
		if err != nil {
			return res, err
		}
		defer func() { _ = tx.Rollback() }()
		r, err := tx.Exec(`DELETE FROM shared_tasks WHERE session_id = ?`, sessionID)
		if err != nil {
			return res, err
		}
		n, _ := r.RowsAffected()
		res.Tasks = int(n)
		r, err = tx.Exec(`DELETE FROM shared_sessions WHERE id = ?`, sessionID)
		if err != nil {
			return res, err
		}
		n, _ = r.RowsAffected()
		res.Sessions = int(n)
		return res, tx.Commit()
	default:
		return res, errors.New("shared_forget: 需要 session_id 或 task_id")
	}
}

// EntryOf 从会话 id 的约定形状里取入口（`web:…` → web；`qq:…` → qq）。
func EntryOf(id string) string {
	if i := strings.IndexByte(id, ':'); i > 0 {
		return id[:i]
	}
	return ""
}

func encodeCaps(caps []string) string {
	if len(caps) == 0 {
		return ""
	}
	raw, err := json.Marshal(caps)
	if err != nil {
		return ""
	}
	return string(raw)
}

func decodeCaps(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	var caps []string
	if err := json.Unmarshal([]byte(raw), &caps); err != nil {
		return nil
	}
	return caps
}
