// Package store 是 batch 的任务账本（SQLite）。
//
// 只存摘要：原始 input 不入库（可能含隐私），完整响应也不入库（需要就再调一次目标动作）。
// 任务与项都在库里，所以服务重启后能给出明确归宿（见 batch/README.md 的「重启行为」）。
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"

	"github.com/NouSart/sump/protocol"
)

// Store 是一个任务账本。
type Store struct {
	db *sql.DB
}

// Job 是一个任务的静态信息与计数。
type Job struct {
	ItemID     string
	Service    string
	Action     string
	Caller     string
	Boss       string
	Trace      string
	Kind       string
	Phase      string
	Total      int
	Ok         int
	Failed     int
	Skipped    int
	KeyTotal   int
	KeyDone    int
	Woken      bool
	Summary    string
	CreatedAt  time.Time
	StartedAt  time.Time
	FinishedAt time.Time

	// 声明的执行策略（重启恢复要照原样接着跑）。
	Concurrency int
	TimeoutMS   int
	OnError     string
	WakeMode    string
}

// Item 是一项的状态与结果摘要。
// Item 是一项的状态与结果摘要。Input 只在任务未结束期间有值（重启恢复用）。
type Item struct {
	Seq     int
	Tag     string
	Key     bool
	Status  string
	Input   []byte
	Error   string
	Preview []byte
	Size    int
	MS      int64
}

// Open 打开（或创建）账本并建表。
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
	// SQLite 单写者：串行化连接，避免并发写锁冲突。
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout=5000;"); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭账本。
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS jobs (
  item_id     TEXT PRIMARY KEY,
  service     TEXT NOT NULL,
  action      TEXT NOT NULL,
  caller      TEXT NOT NULL,
  boss        TEXT NOT NULL DEFAULT '',
  trace       TEXT NOT NULL DEFAULT '',
  kind        TEXT NOT NULL DEFAULT 'once',
  phase       TEXT NOT NULL,
  total       INTEGER NOT NULL DEFAULT 0,
  ok          INTEGER NOT NULL DEFAULT 0,
  failed      INTEGER NOT NULL DEFAULT 0,
  skipped     INTEGER NOT NULL DEFAULT 0,
  key_total   INTEGER NOT NULL DEFAULT 0,
  key_done    INTEGER NOT NULL DEFAULT 0,
  woken       INTEGER NOT NULL DEFAULT 0,
  summary     TEXT NOT NULL DEFAULT '',
  created_at  INTEGER NOT NULL,
  started_at  INTEGER NOT NULL DEFAULT 0,
  finished_at INTEGER NOT NULL DEFAULT 0,
  concurrency INTEGER NOT NULL DEFAULT 0,
  timeout_ms  INTEGER NOT NULL DEFAULT 0,
  on_error    TEXT NOT NULL DEFAULT 'continue',
  wake_mode   TEXT NOT NULL DEFAULT 'all'
);
CREATE INDEX IF NOT EXISTS idx_jobs_created ON jobs(created_at);
CREATE TABLE IF NOT EXISTS items (
  item_id TEXT NOT NULL,
  seq     INTEGER NOT NULL,
  tag     TEXT NOT NULL DEFAULT '',
  is_key  INTEGER NOT NULL DEFAULT 0,
  status  TEXT NOT NULL,
  input   BLOB,
  error   TEXT NOT NULL DEFAULT '',
  preview BLOB NOT NULL DEFAULT x'',
  size    INTEGER NOT NULL DEFAULT 0,
  ms      INTEGER NOT NULL DEFAULT 0,
  PRIMARY KEY (item_id, seq)
);`)
	if err != nil {
		return err
	}
	return s.addMissingJobColumns()
}

// 后来加的列：老账本没有就补上（幂等）。
var jobColumns = []struct{ name, ddl string }{
	{"concurrency", `ALTER TABLE jobs ADD COLUMN concurrency INTEGER NOT NULL DEFAULT 0`},
	{"timeout_ms", `ALTER TABLE jobs ADD COLUMN timeout_ms INTEGER NOT NULL DEFAULT 0`},
	{"on_error", `ALTER TABLE jobs ADD COLUMN on_error TEXT NOT NULL DEFAULT 'continue'`},
	{"wake_mode", `ALTER TABLE jobs ADD COLUMN wake_mode TEXT NOT NULL DEFAULT 'all'`},
}

func (s *Store) addMissingJobColumns() error {
	rows, err := s.db.Query(`PRAGMA table_info(jobs)`)
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for rows.Next() {
		var (
			cid, notNull, pk int
			name, ctype      string
			dflt             any
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
	for _, c := range jobColumns {
		if have[c.name] {
			continue
		}
		if _, err := s.db.Exec(c.ddl); err != nil {
			return err
		}
	}
	return nil
}

// Policy 是一个任务的执行策略。
//
// 必须落库：重启恢复时如果丢掉声明，on_error=stop 会变成 continue、并发会顶到上限、
// 唤醒档位会从 key 变 all——那是换了个任务在跑，不是"从断点继续"。
type Policy struct {
	Concurrency int
	TimeoutMS   int
	OnError     string
	WakeMode    string
}

// CreateJob 落一个新任务：状态 accepted，全部项 pending（Tag/Key/策略一并落下来）。
func (s *Store) CreateJob(itemID, service, action, caller, boss, trace, kind string, pol Policy,
	items []protocol.BatchItem, now time.Time) error {
	keyTotal := 0
	for _, it := range items {
		if it.Key {
			keyTotal++
		}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`INSERT INTO jobs
(item_id, service, action, caller, boss, trace, kind, phase, total, key_total, created_at,
 concurrency, timeout_ms, on_error, wake_mode)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, service, action, caller, boss, trace, kind, protocol.BatchPhaseAccepted,
		len(items), keyTotal, now.UnixMilli(),
		pol.Concurrency, pol.TimeoutMS, pol.OnError, pol.WakeMode); err != nil {
		return err
	}
	for i, it := range items {
		isKey := 0
		if it.Key {
			isKey = 1
		}
		// input 只在任务未结束期间落库：项一终结就抹掉（MarkItem / SkipPending 里清 NULL）。
		// 这是"重启可恢复"与"隐私不落盘"之间的折中——落盘窗口 = 任务存活期。
		if _, err := tx.Exec(`INSERT INTO items (item_id, seq, tag, is_key, status, input) VALUES (?, ?, ?, ?, ?, ?)`,
			itemID, i, it.Tag, isKey, protocol.BatchItemPending, []byte(it.Input)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// SetPhase 改任务阶段（accepted → running → paused/done/failed/cancelled）。
func (s *Store) SetPhase(itemID, phase string) error {
	_, err := s.db.Exec(`UPDATE jobs SET phase = ? WHERE item_id = ?`, phase, itemID)
	return err
}

// MarkStarted 记开始时间与 running。
//
// 只在 accepted 时生效：正常完成与"取消了还没起跑"都可能在它之前落终态，
// 不能把终态又改回 running。
func (s *Store) MarkStarted(itemID string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET phase = ?, started_at = ? WHERE item_id = ? AND phase = ?`,
		protocol.BatchPhaseRunning, now.UnixMilli(), itemID, protocol.BatchPhaseAccepted)
	return err
}

// Finish 落终态、计数与摘要。
func (s *Store) Finish(itemID, phase string, ok, failed, skipped int, summary string, now time.Time) error {
	_, err := s.db.Exec(`UPDATE jobs SET phase = ?, ok = ?, failed = ?, skipped = ?, summary = ?, finished_at = ?
WHERE item_id = ?`, phase, ok, failed, skipped, summary, now.UnixMilli(), itemID)
	return err
}

// SetCounts 用项的真实状态覆盖任务计数（重启恢复后重算用）。
func (s *Store) SetCounts(itemID string, ok, failed, skipped int) error {
	_, err := s.db.Exec(`UPDATE jobs SET ok = ?, failed = ?, skipped = ? WHERE item_id = ?`,
		ok, failed, skipped, itemID)
	return err
}

// SetWoken 标记 key 唤醒已触发。
func (s *Store) SetWoken(itemID string, woken bool) error {
	v := 0
	if woken {
		v = 1
	}
	_, err := s.db.Exec(`UPDATE jobs SET woken = ? WHERE item_id = ?`, v, itemID)
	return err
}

// SetKeyDone 更新已完成的关键项数。
func (s *Store) SetKeyDone(itemID string, n int) error {
	_, err := s.db.Exec(`UPDATE jobs SET key_done = ? WHERE item_id = ?`, n, itemID)
	return err
}

// MarkItem 落一项的终态，并抹掉原始 input。
//
// 抹掉不是可选项：落盘窗口只覆盖任务存活期，项一终结就没有"还要重跑"的可能，
// 留着就是纯隐私负担。
//
// preview 列是 NOT NULL DEFAULT x”：直接写 nil 会被 SQLite 拒（DEFAULT 只在"没写这列"
// 时生效），失败项本来就没有摘要，所以这里显式归一成空 BLOB。
func (s *Store) MarkItem(itemID string, it Item) error {
	preview := it.Preview
	if preview == nil {
		preview = []byte{}
	}
	_, err := s.db.Exec(`UPDATE items SET status = ?, error = ?, preview = ?, size = ?, ms = ?, input = NULL
WHERE item_id = ? AND seq = ?`, it.Status, it.Error, preview, it.Size, it.MS, itemID, it.Seq)
	return err
}

// SetItemStatus 只改状态（running / skipped / pending）。
func (s *Store) SetItemStatus(itemID string, seq int, status string) error {
	_, err := s.db.Exec(`UPDATE items SET status = ? WHERE item_id = ? AND seq = ?`, status, itemID, seq)
	return err
}

// SkipPending 把未开始的项全部转 skipped（取消 / 失败即停）。
func (s *Store) SkipPending(itemID string) (int, error) {
	res, err := s.db.Exec(`UPDATE items SET status = ?, input = NULL WHERE item_id = ? AND status = ?`,
		protocol.BatchItemSkipped, itemID, protocol.BatchItemPending)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// Job 读一个任务。
func (s *Store) Job(itemID string) (Job, bool, error) {
	row := s.db.QueryRow(`SELECT item_id, service, action, caller, boss, trace, kind, phase,
total, ok, failed, skipped, key_total, key_done, woken, summary, created_at, started_at, finished_at,
concurrency, timeout_ms, on_error, wake_mode
FROM jobs WHERE item_id = ?`, itemID)
	var j Job
	var created, started, finished int64
	var woken int
	err := row.Scan(&j.ItemID, &j.Service, &j.Action, &j.Caller, &j.Boss, &j.Trace, &j.Kind, &j.Phase,
		&j.Total, &j.Ok, &j.Failed, &j.Skipped, &j.KeyTotal, &j.KeyDone, &woken, &j.Summary,
		&created, &started, &finished, &j.Concurrency, &j.TimeoutMS, &j.OnError, &j.WakeMode)
	if err == sql.ErrNoRows {
		return Job{}, false, nil
	}
	if err != nil {
		return Job{}, false, err
	}
	j.Woken = woken != 0
	j.CreatedAt = time.UnixMilli(created)
	if started > 0 {
		j.StartedAt = time.UnixMilli(started)
	}
	if finished > 0 {
		j.FinishedAt = time.UnixMilli(finished)
	}
	return j, true, nil
}

// Items 取一项或多项；status 非空时按状态过滤。
func (s *Store) Items(itemID, status string, offset, limit int) ([]Item, int, error) {
	where := "item_id = ?"
	args := []any{itemID}
	if status != "" {
		where += " AND status = ?"
		args = append(args, status)
	}
	var matched int
	if err := s.db.QueryRow("SELECT COUNT(*) FROM items WHERE "+where, args...).Scan(&matched); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query("SELECT seq, tag, is_key, status, error, preview, size, ms FROM items WHERE "+
		where+" ORDER BY seq LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		var it Item
		var key int
		if err := rows.Scan(&it.Seq, &it.Tag, &key, &it.Status, &it.Error, &it.Preview, &it.Size, &it.MS); err != nil {
			return nil, 0, err
		}
		it.Key = key != 0
		out = append(out, it)
	}
	return out, matched, rows.Err()
}

// PendingItems 返回某任务里仍可执行的项（pending / running），带 input——重启恢复用。
func (s *Store) PendingItems(itemID string) ([]Item, error) {
	rows, err := s.db.Query(`SELECT seq, tag, is_key, status, ifnull(input, x'') FROM items
WHERE item_id = ? AND status IN (?, ?) ORDER BY seq`, itemID, protocol.BatchItemPending, protocol.BatchItemRunning)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Item
	for rows.Next() {
		var it Item
		var key int
		if err := rows.Scan(&it.Seq, &it.Tag, &key, &it.Status, &it.Input); err != nil {
			return nil, err
		}
		it.Key = key != 0
		out = append(out, it)
	}
	return out, rows.Err()
}

// List 按阶段列任务（"" = 全部），新的在前。
func (s *Store) List(phase, caller string, limit int) ([]Job, error) {
	where := "1=1"
	var args []any
	if phase != "" {
		where += " AND phase = ?"
		args = append(args, phase)
	}
	if caller != "" {
		where += " AND caller = ?"
		args = append(args, caller)
	}
	args = append(args, limit)
	rows, err := s.db.Query(`SELECT item_id, service, action, caller, boss, trace, kind, phase,
total, ok, failed, skipped, key_total, key_done, woken, summary, created_at, started_at, finished_at
FROM jobs WHERE `+where+` ORDER BY created_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		var j Job
		var created, started, finished int64
		var woken int
		if err := rows.Scan(&j.ItemID, &j.Service, &j.Action, &j.Caller, &j.Boss, &j.Trace, &j.Kind, &j.Phase,
			&j.Total, &j.Ok, &j.Failed, &j.Skipped, &j.KeyTotal, &j.KeyDone, &woken, &j.Summary,
			&created, &started, &finished); err != nil {
			return nil, err
		}
		j.Woken = woken != 0
		j.CreatedAt = time.UnixMilli(created)
		if started > 0 {
			j.StartedAt = time.UnixMilli(started)
		}
		if finished > 0 {
			j.FinishedAt = time.UnixMilli(finished)
		}
		out = append(out, j)
	}
	return out, rows.Err()
}

// Running 返回所有非终态任务（服务重启时转 paused、兜底判断都用它）。
func (s *Store) Running() ([]Job, error) {
	rows, err := s.db.Query(`SELECT item_id, service, action, caller, boss, trace, kind, phase,
total, ok, failed, skipped, key_total, key_done, woken, summary, created_at, started_at, finished_at
FROM jobs WHERE phase IN (?, ?, ?) ORDER BY created_at`, protocol.BatchPhaseAccepted,
		protocol.BatchPhaseRunning, protocol.BatchPhasePaused)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []Job
	for rows.Next() {
		var j Job
		var created, started, finished int64
		var woken int
		if err := rows.Scan(&j.ItemID, &j.Service, &j.Action, &j.Caller, &j.Boss, &j.Trace, &j.Kind, &j.Phase,
			&j.Total, &j.Ok, &j.Failed, &j.Skipped, &j.KeyTotal, &j.KeyDone, &woken, &j.Summary,
			&created, &started, &finished); err != nil {
			return nil, err
		}
		j.Woken = woken != 0
		j.CreatedAt = time.UnixMilli(created)
		out = append(out, j)
	}
	return out, rows.Err()
}

// Counts 汇总队列里各阶段的任务数。
func (s *Store) Counts() (protocol.BatchCounts, error) {
	var c protocol.BatchCounts
	rows, err := s.db.Query(`SELECT phase, COUNT(*) FROM jobs GROUP BY phase`)
	if err != nil {
		return c, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var phase string
		var n int
		if err := rows.Scan(&phase, &n); err != nil {
			return c, err
		}
		switch phase {
		case protocol.BatchPhaseRunning, protocol.BatchPhaseAccepted:
			c.Running += n
		case protocol.BatchPhasePaused:
			c.Paused += n
		case protocol.BatchPhaseDone:
			c.Done += n
		case protocol.BatchPhaseFailed, protocol.BatchPhaseCancelled:
			c.Failed += n
		}
	}
	return c, rows.Err()
}

// Prune 删掉超过 ttl 的终态任务（连同明细）。
//
// 只清终态：暂停中的任务即便很老也留着——它还没"结束"，清掉等于替用户放弃。
func (s *Store) Prune(now time.Time, ttl time.Duration) (int, error) {
	cutoff := now.Add(-ttl).UnixMilli()
	res, err := s.db.Exec(`DELETE FROM jobs WHERE finished_at > 0 AND finished_at < ?`, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if _, err := s.db.Exec(`DELETE FROM items WHERE item_id NOT IN (SELECT item_id FROM jobs)`); err != nil {
		return int(n), fmt.Errorf("清理孤儿明细失败: %w", err)
	}
	return int(n), nil
}

// MarkInterrupted 把重启时"正在跑"的项标成失败（结果未知，不擅自重跑），并抹掉原始输入。
func (s *Store) MarkInterrupted(itemID, reason string) (int, error) {
	res, err := s.db.Exec(`UPDATE items SET status = ?, error = ?, input = NULL WHERE item_id = ? AND status = ?`,
		protocol.BatchItemFailed, reason, itemID, protocol.BatchItemRunning)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// CountByStatus 给出某任务各状态的项数（汇总与判断"是否全部结束"用）。
func (s *Store) CountByStatus(itemID string) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT status, COUNT(*) FROM items WHERE item_id = ? GROUP BY status`, itemID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var st string
		var n int
		if err := rows.Scan(&st, &n); err != nil {
			return nil, err
		}
		out[st] = n
	}
	return out, rows.Err()
}
