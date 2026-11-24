package store

// 记忆中心 v2 schema / 版本闸门的包内测试（P1：回执、call 账本、序号计数器；
// P2：context 快照与触发态）。
//
// 版本闸门是「不迁移、重置」策略的入口（S-001 §11）：最关键的一条不是「旧库被拒绝」，
// 而是**拒绝发生在任何 DDL 之前**——旧库被打开失败后必须原封不动，否则一次误启动
// 就把旧库改成了既不是旧格式也不是新格式的样子。下面用「比对文件字节」把这条钉住。

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// v2TableNames 是 v2 真正建出来的表（D7 的 work_context_grants / image_index 不在）。
var v2TableNames = []string{
	"store_receipts",
	"report_receipts",
	"context_snapshots",
	"trigger_states",
	"calls",
	"call_controls",
	"seq_counters",
}

// openV2Store 在临时目录开一个新库（避免与其它测试文件里的 helper 重名）。
func openV2Store(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatalf("打开新库失败: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// openRawDB 直接开一个句柄（测试里手工造旧库 / 检查落盘状态用）。
func openRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("直接打开 %s 失败: %v", path, err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// pragmaInt 读一个返回单整数的 PRAGMA（如 user_version）。
func pragmaInt(t *testing.T, db *sql.DB, query string) int {
	t.Helper()
	var v int
	if err := db.QueryRow(query).Scan(&v); err != nil {
		t.Fatalf("%s: %v", query, err)
	}
	return v
}

// tableExists 判断库里有没有某张用户表。
func tableExists(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name,
	).Scan(&n); err != nil {
		t.Fatalf("查表 %s 是否存在: %v", name, err)
	}
	return n > 0
}

// columnNames 返回一张表的列名集合。
func columnNames(t *testing.T, db *sql.DB, table string) map[string]bool {
	t.Helper()
	rows, err := db.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		t.Fatalf("读表 %s 结构: %v", table, err)
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
			t.Fatalf("读表 %s 结构: %v", table, err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("读表 %s 结构: %v", table, err)
	}
	return have
}

// userTableCount 统计用户表数量（排除 sqlite_ 内部表）。
func userTableCount(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%'`,
	).Scan(&n); err != nil {
		t.Fatalf("统计用户表: %v", err)
	}
	return n
}

// TestSchemaV2NewDatabase 新库：版本 = 1、v2 的表 / 新列 / 唯一索引都建好。
func TestSchemaV2NewDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	st, err := Open(path)
	if err != nil {
		t.Fatalf("打开新库失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	if got := pragmaInt(t, st.db, `PRAGMA user_version`); got != SchemaVersion {
		t.Fatalf("新库 user_version = %d，期望 %d", got, SchemaVersion)
	}
	// 表名与列名逐张钉住（字段按 S-001 §4.3 / §4.5 / §4.7）。
	wantColumns := map[string][]string{
		"store_receipts":    {"work", "client_item_id", "payload_hash", "item_id", "created_at"},
		"report_receipts":   {"work", "source_service", "report_id", "payload_hash", "item_id", "call_id", "report_seq", "phase", "final", "data", "created_at"},
		"context_snapshots": {"snapshot_id", "work", "trigger_kind", "trigger_item", "payload", "schema", "created_at", "expires_at"},
		"trigger_states":    {"work", "trigger_kind", "trigger_item", "state", "snapshot_id", "completed_at", "created_at", "updated_at"},
		"calls":             {"id", "work", "service", "action", "mode", "pausable", "ttl_ms", "exec_deadline_ms", "remote_item_id", "observe_state", "exec_state", "phase", "reason", "result_ref", "remote_may_continue", "created_at", "updated_at"},
		"call_controls":     {"control_id", "call_id", "work", "op", "ttl_ms", "observe", "exec", "reason", "created_at", "updated_at"},
		"seq_counters":      {"conversation_id", "last_seq"},
	}
	for _, name := range v2TableNames {
		if !tableExists(t, st.db, name) {
			t.Errorf("新库缺少表 %s", name)
			continue
		}
		have := columnNames(t, st.db, name)
		for _, column := range wantColumns[name] {
			if !have[column] {
				t.Errorf("表 %s 缺少列 %s", name, column)
			}
		}
		if len(have) != len(wantColumns[name]) {
			t.Errorf("表 %s 列数 = %d，期望 %d（%v）", name, len(have), len(wantColumns[name]), have)
		}
	}
	// D7 的表不在 P1–P5：建了就是没有消费者的第二份真相。
	for _, name := range []string{"work_context_grants", "image_index"} {
		if tableExists(t, st.db, name) {
			t.Errorf("P1 不应建表 %s", name)
		}
	}
	for _, c := range []struct{ table, column string }{
		{"works", "task"},
		{"messages", "work_id"},
		{"messages", "visibility"},
	} {
		if !columnNames(t, st.db, c.table)[c.column] {
			t.Errorf("表 %s 缺少新列 %s", c.table, c.column)
		}
	}
	// 唯一索引：report_seq 去重 + calls 的部分唯一索引 + work 维度消息索引。
	for _, idx := range []string{"uq_report_seq", "uq_calls_remote", "idx_messages_work"} {
		var n int
		if err := st.db.QueryRow(
			`SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = ?`, idx,
		).Scan(&n); err != nil {
			t.Fatalf("查索引 %s: %v", idx, err)
		}
		if n != 1 {
			t.Errorf("缺少索引 %s", idx)
		}
	}
	var idxSQL string
	if err := st.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type = 'index' AND name = 'uq_calls_remote'`).Scan(&idxSQL); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(idxSQL, "WHERE remote_item_id IS NOT NULL") {
		t.Errorf("uq_calls_remote 应是部分唯一索引，实际: %s", idxSQL)
	}

	// 新列的默认值：works.task = ''，messages.work_id = ''；
	// visibility 默认 'work'——与老路径 recordVia 对空值的归一一致（未标注 = 按 work 可见）。
	if _, err := st.db.Exec(`INSERT INTO messages(conversation_id, role, content, created_at)
		VALUES('c', 'user', '只有老列', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	var workID, visibility string
	if err := st.db.QueryRow(`SELECT work_id, visibility FROM messages WHERE conversation_id = 'c'`).Scan(&workID, &visibility); err != nil {
		t.Fatal(err)
	}
	if workID != "" || visibility != "work" {
		t.Errorf("messages 新列默认值 = (%q,%q)，期望 (\"\",\"work\")", workID, visibility)
	}
}

// TestSchemaV2ReportReceiptKeys report_receipts 的两层去重：
// 主键 (work,source_service,report_id) 拦同 ID 重放；
// 唯一索引 (work,source_service,call_id,report_seq) 拦「换 report_id 重放」。
func TestSchemaV2ReportReceiptKeys(t *testing.T) {
	st := openV2Store(t)
	insert := func(work, sourceService, reportID, callID string, reportSeq int64) error {
		_, err := st.db.Exec(`INSERT INTO report_receipts
			(work, source_service, report_id, payload_hash, item_id, call_id, report_seq, created_at)
			VALUES(?,?,?,?,?,?,?,?)`,
			work, sourceService, reportID, "hash", "item-1", callID, reportSeq, "2026-01-01T00:00:00Z")
		return err
	}
	if err := insert("w1", "shell", "r1", "call-1", 1); err != nil {
		t.Fatalf("首次写入失败: %v", err)
	}
	if err := insert("w1", "shell", "r1", "call-1", 2); err == nil {
		t.Fatal("同 (work,source_service,report_id) 应被主键拦截")
	}
	if err := insert("w1", "shell", "r2", "call-1", 1); err == nil {
		t.Fatal("换 report_id 但同 (work,source_service,call_id,report_seq) 应被唯一索引拦截")
	}
	// 不同 call / 不同 seq / 不同 work 都该放行。
	if err := insert("w1", "shell", "r3", "call-2", 1); err != nil {
		t.Errorf("不同 call 同 seq 应允许: %v", err)
	}
	if err := insert("w1", "shell", "r4", "call-1", 2); err != nil {
		t.Errorf("同 call 不同 seq 应允许: %v", err)
	}
	if err := insert("w2", "shell", "r1", "call-1", 1); err != nil {
		t.Errorf("不同 work 应允许: %v", err)
	}
}

// TestSchemaV2CallsRemoteIndex calls.remote_item_id 可空 + 部分唯一索引：
// 多个未受理（NULL）的调用能共存，受理后回填的句柄撞键要被拦。
func TestSchemaV2CallsRemoteIndex(t *testing.T) {
	st := openV2Store(t)
	insert := func(id string, remote any) error {
		_, err := st.db.Exec(`INSERT INTO calls(id, work, service, remote_item_id, created_at, updated_at)
			VALUES(?,?,?,?,?,?)`, id, "w1", "shell", remote, "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
		return err
	}
	// 受理前：remote_item_id 为 NULL，多个调用不得互撞。
	if err := insert("call-1", nil); err != nil {
		t.Fatalf("NULL remote_item_id 首次写入失败: %v", err)
	}
	if err := insert("call-2", nil); err != nil {
		t.Fatalf("多个 NULL remote_item_id 应允许共存: %v", err)
	}
	// 受理后回填句柄：同 (service,work,remote_item_id) 不许重复。
	if err := insert("call-3", "remote-1"); err != nil {
		t.Fatalf("回填 remote_item_id 失败: %v", err)
	}
	if err := insert("call-4", "remote-1"); err == nil {
		t.Fatal("同 (service,work,remote_item_id) 应被部分唯一索引拦截")
	}
}

// TestSchemaV2RejectsLegacyDatabaseBeforeDDL 旧库（user_version=0 且已有表）：
// 返回 ErrSchemaVersionMismatch，并且**不做任何 DDL**——文件字节不变、新表一张都没建。
func TestSchemaV2RejectsLegacyDatabaseBeforeDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE works (id TEXT PRIMARY KEY, status TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if st != nil {
		_ = st.Close()
		t.Fatal("旧库不应被打开成功")
	}
	if err == nil {
		t.Fatal("旧库应返回版本不匹配错误")
	}
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("错误应可 errors.Is(ErrSchemaVersionMismatch) 判定，实际: %v", err)
	}
	var mismatch *SchemaVersionMismatchError
	if !errors.As(err, &mismatch) {
		t.Fatalf("错误应是 *SchemaVersionMismatchError，实际 %T: %v", err, err)
	}
	if mismatch.Path != path || mismatch.Current != 0 || mismatch.Expected != SchemaVersion {
		t.Fatalf("版本错误详情不对: %+v", mismatch)
	}
	for _, want := range []string{
		"当前版本 0",
		fmt.Sprintf("期望版本 %d", SchemaVersion),
		"data/memory.db",
		"data/experience.db",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q: %v", want, err)
		}
	}

	// 失败之后库必须原样：文件字节一致、版本仍是 0、没有新表、works 也没被补列。
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("拒绝旧库时改写了库文件：DDL 发生在版本检查之前")
	}
	db = openRawDB(t, path)
	if got := pragmaInt(t, db, `PRAGMA user_version`); got != 0 {
		t.Errorf("旧库 user_version 被改成 %d，应保持 0", got)
	}
	for _, name := range v2TableNames {
		if tableExists(t, db, name) {
			t.Errorf("拒绝旧库后不应存在表 %s（DDL 必须在检查之后）", name)
		}
	}
	if !tableExists(t, db, "works") {
		t.Fatal("旧库原有的 works 表不该消失")
	}
	if columnNames(t, db, "works")["task"] {
		t.Error("拒绝旧库时不应给 works 补 task 列")
	}
}

// TestSchemaV2RejectsNewerVersion 版本高于 SchemaVersion：拒绝，并不动库。
func TestSchemaV2RejectsNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(`CREATE TABLE marker(x TEXT); PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if st != nil {
		_ = st.Close()
		t.Fatal("版本过高的库不应被打开成功")
	}
	if err == nil {
		t.Fatal("版本过高的库应返回错误")
	}
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("应可 errors.Is(ErrSchemaVersionMismatch) 判定，实际: %v", err)
	}
	if !strings.Contains(err.Error(), fmt.Sprintf("当前版本 %d", SchemaVersion+1)) ||
		!strings.Contains(err.Error(), fmt.Sprintf("期望版本 %d", SchemaVersion)) {
		t.Fatalf("错误信息应含两个版本号: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("拒绝版本过高的库时改写了库文件")
	}
}

// TestSchemaV2CheckPath 只读预检：不存在 / 旧库 / 新库三种状态，且绝不写库。
func TestSchemaV2CheckPath(t *testing.T) {
	t.Run("不存在", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "absent.db")
		exists, version, err := CheckPath(path)
		if err != nil || exists || version != 0 {
			t.Fatalf("CheckPath(不存在) = (%v,%d,%v)，期望 (false,0,nil)", exists, version, err)
		}
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("CheckPath 不应创建库文件: %v", statErr)
		}
	})

	t.Run("旧库", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "old.db")
		db, err := sql.Open("sqlite", path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`CREATE TABLE works (id TEXT PRIMARY KEY)`); err != nil {
			t.Fatal(err)
		}
		if err := db.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		exists, version, err := CheckPath(path)
		if err != nil || !exists || version != 0 {
			t.Fatalf("CheckPath(旧库) = (%v,%d,%v)，期望 (true,0,nil)", exists, version, err)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("CheckPath 改写了旧库文件（预检必须只读）")
		}
	})

	t.Run("新库", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "memory.db")
		st, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Close(); err != nil {
			t.Fatal(err)
		}
		before, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		exists, version, err := CheckPath(path)
		if err != nil || !exists || version != SchemaVersion {
			t.Fatalf("CheckPath(新库) = (%v,%d,%v)，期望 (true,%d,nil)", exists, version, err, SchemaVersion)
		}
		after, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(before, after) {
			t.Error("CheckPath 改写了新库文件（预检必须只读）")
		}
	})
}

// TestSchemaV2MigratesMatchingVersion 同版本的库只做幂等增量迁移：
// 手工造一个「上个版本」的库（基础表齐、没有 v2 表和 v2 新列、user_version=1），
// Open 应补上表/列/索引，而不是当成旧库拒绝。
func TestSchemaV2MigratesMatchingVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for _, ddl := range []string{schema, workSchema, a2aSchema} {
		if _, err := db.Exec(ddl); err != nil {
			_ = db.Close()
			t.Fatalf("造旧版本库失败: %v", err)
		}
	}
	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, SchemaVersion)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path)
	if err != nil {
		t.Fatalf("同版本库应允许幂等迁移，实际失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	for _, name := range v2TableNames {
		if !tableExists(t, st.db, name) {
			t.Errorf("增量迁移后缺少表 %s", name)
		}
	}
	for _, c := range []struct{ table, column string }{
		{"works", "task"},
		{"messages", "work_id"},
		{"messages", "visibility"},
	} {
		if !columnNames(t, st.db, c.table)[c.column] {
			t.Errorf("增量迁移后表 %s 缺少列 %s", c.table, c.column)
		}
	}
	if got := pragmaInt(t, st.db, `PRAGMA user_version`); got != SchemaVersion {
		t.Errorf("增量迁移后 user_version = %d，期望 %d", got, SchemaVersion)
	}
}

// TestSchemaV2EmptyFileTreatedAsNew 已存在但完全空白的文件等同新库：允许建表。
func TestSchemaV2EmptyFileTreatedAsNew(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("空文件应按新库处理，实际失败: %v", err)
	}
	defer func() { _ = st.Close() }()
	if got := pragmaInt(t, st.db, `PRAGMA user_version`); got != SchemaVersion {
		t.Fatalf("空文件建库后 user_version = %d，期望 %d", got, SchemaVersion)
	}
	for _, name := range v2TableNames {
		if !tableExists(t, st.db, name) {
			t.Errorf("空文件建库后缺少表 %s", name)
		}
	}
}

// TestSchemaV2RelativePath 生产默认是相对路径（data/memory.db）：
// Open / CheckPath 的只读 DSN 都不能依赖绝对路径，且 Open 要能补建父目录。
func TestSchemaV2RelativePath(t *testing.T) {
	t.Chdir(t.TempDir())
	path := filepath.Join("data", "memory.db")

	if exists, version, err := CheckPath(path); err != nil || exists || version != 0 {
		t.Fatalf("CheckPath(相对路径/不存在) = (%v,%d,%v)，期望 (false,0,nil)", exists, version, err)
	}
	st, err := Open(path)
	if err != nil {
		t.Fatalf("相对路径打开新库失败: %v", err)
	}
	if got := pragmaInt(t, st.db, `PRAGMA user_version`); got != SchemaVersion {
		t.Errorf("相对路径新库 user_version = %d，期望 %d", got, SchemaVersion)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	if exists, version, err := CheckPath(path); err != nil || !exists || version != SchemaVersion {
		t.Fatalf("CheckPath(相对路径/新库) = (%v,%d,%v)，期望 (true,%d,nil)", exists, version, err, SchemaVersion)
	}
}

// TestSchemaV2OpenIsIdempotent 重复 Open：不报错、不重复建表、已有数据还在。
func TestSchemaV2OpenIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	firstCount := userTableCount(t, st.db)
	if err := st.EnsureConversation("c1", "重复打开", "frontend"); err != nil {
		t.Fatal(err)
	}
	if err := st.Append("c1", "user", "别弄丢我"); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	st, err = Open(path)
	if err != nil {
		t.Fatalf("第二次 Open 失败: %v", err)
	}
	defer func() { _ = st.Close() }()

	if got := userTableCount(t, st.db); got != firstCount {
		t.Errorf("重复 Open 后用户表数量 = %d，期望 %d（不该重复建表）", got, firstCount)
	}
	if got := pragmaInt(t, st.db, `PRAGMA user_version`); got != SchemaVersion {
		t.Errorf("重复 Open 后 user_version = %d，期望 %d", got, SchemaVersion)
	}
	hist, err := st.History("c1", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Content != "别弄丢我" {
		t.Fatalf("重复 Open 后老数据不对: %+v", hist)
	}
}
