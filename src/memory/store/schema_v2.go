package store

// 记忆中心 v2 的 schema 与版本闸门（S-001 §4 / §11）。
//
// 版本策略是「不迁移、重置」：库文件里的 user_version 必须等于 SchemaVersion。
// 旧库（user_version=0 且已有表）一律拒绝启动，而且判断必须在**任何 DDL 之前**完成——
// 否则一次失败的启动就会把旧库改成既不是旧格式、也不是新格式的样子（§11.1）。
// 拒绝只提示删库或改名备份，不自动删、不自动改。

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// SchemaVersion 是 memory 会话库当前的数据格式版本。
// 表结构一变就要 +1；版本不等于它时只允许「新库」或「同版本幂等增量迁移」，其余拒绝。
const SchemaVersion = 1

// ErrSchemaVersionMismatch 是版本闸门的哨兵错误，用 errors.Is 判定。
var ErrSchemaVersionMismatch = errors.New("schema 版本不匹配")

// SchemaVersionMismatchError 带上路径与两个版本号，错误信息直接给人看。
type SchemaVersionMismatchError struct {
	Path     string // 出问题的库文件路径
	Current  int    // 库里记录的版本（PRAGMA user_version）
	Expected int    // 本程序期望的版本
}

func (e *SchemaVersionMismatchError) Error() string {
	return fmt.Sprintf("会话库 %s 的 schema 版本不匹配：当前版本 %d，期望版本 %d；"+
		"需删除 data/memory.db 与 data/experience.db 后重启（可先改名备份再重建，不会自动删除）",
		e.Path, e.Current, e.Expected)
}

// Is 让 errors.Is(err, ErrSchemaVersionMismatch) 成立（版本过高也走同一个闸门）。
func (e *SchemaVersionMismatchError) Is(target error) bool {
	return target == ErrSchemaVersionMismatch
}

// schemaState 是只读预检得到的库状态。
type schemaState struct {
	exists    bool // 库文件是否存在
	version   int  // PRAGMA user_version
	hasTables bool // sqlite_master 里是否已有用户表
}

// CheckPath 只做 os.Stat + 只读读取 user_version，不建表、不写库。
// 给 main 的启动预检用（S-001 §11.1）：文件不存在返回 (false, 0, nil)，
// 存在则返回真实版本号，供调用方决定「新库 / 版本一致 / 拒绝启动」。
func CheckPath(path string) (exists bool, version int, err error) {
	if _, statErr := os.Stat(path); statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("检查会话库 %s: %w", path, statErr)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return true, 0, err
	}
	defer func() { _ = db.Close() }()
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return true, 0, fmt.Errorf("读取会话库 %s 的 schema 版本: %w", path, err)
	}
	return true, version, nil
}

// inspectSchema 是 Open 的预检：文件不存在 / 版本 / 是否已有表，全部只读完成。
func inspectSchema(path string) (schemaState, error) {
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return schemaState{}, nil
		}
		return schemaState{}, fmt.Errorf("检查会话库 %s: %w", path, err)
	}
	db, err := openReadOnly(path)
	if err != nil {
		return schemaState{}, err
	}
	defer func() { _ = db.Close() }()

	state := schemaState{exists: true}
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&state.version); err != nil {
		return schemaState{}, fmt.Errorf("读取会话库 %s 的 schema 版本: %w", path, err)
	}
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`).Scan(&n); err != nil {
		return schemaState{}, fmt.Errorf("读取会话库 %s 的表清单: %w", path, err)
	}
	state.hasTables = n > 0
	return state, nil
}

// openReadOnly 以只读方式打开库（file: URI + mode=ro），绝不会创建文件或写库。
// 调用方负责 Close。mode=ro 下 SQLite 会直接拒绝任何写语句，是比「只读事务」更硬的保证。
func openReadOnly(path string) (*sql.DB, error) {
	// filepath.ToSlash 让 Windows 的 `C:\a\b.db` 变成 URI 路径 `C:/a/b.db`。
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("只读打开会话库 %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("只读打开会话库 %s: %w", path, err)
	}
	return db, nil
}

// applySchema 在单个事务里建表 / 补表，并写入 user_version。
// 表定义全是 IF NOT EXISTS，所以对同版本的库是幂等的增量迁移；
// 对新库则是「建表 + 写版本」同一事务（§11.1 第 5 条——不能出现建了一半、版本没写的库）。
func applySchema(db *sql.DB) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("开始 schema 事务: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for _, part := range []struct {
		name string
		ddl  string
	}{
		{"会话库基础表", schema},
		{"work 账本", workSchema},
		{"A2A 队列", a2aSchema},
		{"记忆中心 v2", v2Schema},
	} {
		if _, err := tx.Exec(part.ddl); err != nil {
			return fmt.Errorf("建表失败（%s）: %w", part.name, err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion)); err != nil {
		return fmt.Errorf("写入 schema 版本 %d: %w", SchemaVersion, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("提交 schema 事务: %w", err)
	}
	return nil
}

// v2Schema 是记忆中心 v2 新增的表（S-001 §4.3–§4.7）。
// 只做加法：既有表的新列在 migrate 里用 PRAGMA table_info 判断后补，不在这里 ALTER。
const v2Schema = `
CREATE TABLE IF NOT EXISTS image_index (
  owner           TEXT NOT NULL,
  conversation_id TEXT NOT NULL,
  image_id        TEXT NOT NULL,
  gist            TEXT NOT NULL DEFAULT '',
  status          TEXT NOT NULL,
  first_seen_at   TEXT NOT NULL,
  last_viewed_at  TEXT NOT NULL,
  PRIMARY KEY (owner, conversation_id, image_id)
);
CREATE TABLE IF NOT EXISTS store_receipts (
  work           TEXT NOT NULL,
  client_item_id TEXT NOT NULL,
  payload_hash   TEXT NOT NULL,
  item_id        TEXT NOT NULL,
  created_at     TEXT NOT NULL,
  PRIMARY KEY (work, client_item_id)
);
CREATE TABLE IF NOT EXISTS report_receipts (
  work           TEXT NOT NULL,
  source_service TEXT NOT NULL,
  report_id      TEXT NOT NULL,
  payload_hash   TEXT NOT NULL,
  item_id        TEXT NOT NULL,
  call_id        TEXT NOT NULL,
  report_seq     INTEGER NOT NULL,
  phase          TEXT NOT NULL DEFAULT '',
  final          INTEGER NOT NULL DEFAULT 0,
  data           BLOB,
  created_at     TEXT NOT NULL,
  PRIMARY KEY (work, source_service, report_id)
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_report_seq
  ON report_receipts(work, source_service, call_id, report_seq);
CREATE TABLE IF NOT EXISTS context_snapshots (
  snapshot_id  TEXT PRIMARY KEY,
  work         TEXT NOT NULL,
  trigger_kind TEXT NOT NULL,
  trigger_item TEXT NOT NULL,
  payload      BLOB NOT NULL,
  schema       TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  expires_at   TEXT NOT NULL,
  UNIQUE(work, trigger_kind, trigger_item)
);
CREATE TABLE IF NOT EXISTS trigger_states (
  work         TEXT NOT NULL,
  trigger_kind TEXT NOT NULL,
  trigger_item TEXT NOT NULL,
  state        TEXT NOT NULL,
  snapshot_id  TEXT,
  completed_at TEXT,
  created_at   TEXT NOT NULL,
  updated_at   TEXT NOT NULL,
  PRIMARY KEY (work, trigger_kind, trigger_item)
);
CREATE TABLE IF NOT EXISTS calls (
  id                  TEXT PRIMARY KEY,
  work                TEXT NOT NULL,
  service             TEXT NOT NULL,
  action              TEXT NOT NULL DEFAULT '',
  mode                TEXT NOT NULL DEFAULT 'once',
  pausable            INTEGER NOT NULL DEFAULT 0,
  ttl_ms              INTEGER NOT NULL DEFAULT 0,
  exec_deadline_ms    INTEGER NOT NULL DEFAULT 0,
  remote_item_id      TEXT,
  observe_state       TEXT NOT NULL DEFAULT 'waiting',
  exec_state          TEXT NOT NULL DEFAULT 'created',
  phase               TEXT,
  reason              TEXT NOT NULL DEFAULT '',
  result_ref          TEXT NOT NULL DEFAULT '',
  remote_may_continue INTEGER NOT NULL DEFAULT 0,
  created_at          TEXT NOT NULL,
  updated_at          TEXT NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS uq_calls_remote
  ON calls(service, work, remote_item_id) WHERE remote_item_id IS NOT NULL;
CREATE TABLE IF NOT EXISTS call_controls (
  control_id TEXT PRIMARY KEY,
  call_id    TEXT NOT NULL,
  work       TEXT NOT NULL DEFAULT '',
  op         TEXT NOT NULL,
  ttl_ms     INTEGER NOT NULL DEFAULT 0,
  observe    TEXT NOT NULL DEFAULT '',
  exec       TEXT NOT NULL DEFAULT '',
  reason     TEXT NOT NULL DEFAULT '',
  created_at TEXT NOT NULL,
  updated_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS work_context_grants (
  grantee_work TEXT NOT NULL,
  ref_kind     TEXT NOT NULL,
  ref_id       TEXT NOT NULL,
  granted_by   TEXT NOT NULL,
  created_at   TEXT NOT NULL,
  PRIMARY KEY (grantee_work, ref_kind, ref_id)
);
CREATE TABLE IF NOT EXISTS seq_counters (
  conversation_id TEXT PRIMARY KEY,
  last_seq        INTEGER NOT NULL DEFAULT 0
);
`
