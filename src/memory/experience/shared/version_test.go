package shared

// 共享库版本闸门的四条验收（R-023 D6）：旧库拒绝且字节不变、版本过高拒绝、
// 新库放行并写版本、预检不建文件。与会话库的 schema 闸门用同一组条件。

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestSharedRejectsLegacyDatabaseBeforeDDL 旧库（user_version=0 且已有表）：
// 返回 ErrSchemaVersionMismatch，并且**不做任何 DDL**——文件字节不变、共享表一张都没建。
func TestSharedRejectsLegacyDatabaseBeforeDDL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY, content TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, time.Now())
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
		"手动删除或改名",
		path,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("错误信息缺少 %q: %v", want, err)
		}
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("拒绝旧库时改写了库文件：DDL 发生在版本检查之前")
	}
	db = openSharedRawDB(t, path)
	if got := pragmaVersion(t, db); got != 0 {
		t.Errorf("旧库 user_version 被改成 %d，应保持 0", got)
	}
	if tableExistsShared(t, db, "shared_sessions") {
		t.Error("拒绝旧库后不应建 shared_sessions 表（DDL 必须在检查之后）")
	}
	if !tableExistsShared(t, db, "memories") {
		t.Fatal("旧库原有的 memories 表不该消失")
	}
}

// TestSharedRejectsNewerVersion 版本高于 SchemaVersion：拒绝，并不动库。
func TestSharedRejectsNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "future.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf(
		`CREATE TABLE marker(x TEXT); PRAGMA user_version = %d`, SchemaVersion+1)); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, time.Now())
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

// TestSharedNewDatabaseWritesVersion 新库放行：建共享表并在同一事务里写 user_version。
func TestSharedNewDatabaseWritesVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experience.db")
	st, err := Open(path, time.Now())
	if err != nil {
		t.Fatalf("新库应允许打开: %v", err)
	}
	if got := pragmaVersion(t, st.DB()); got != SchemaVersion {
		t.Errorf("新库 user_version = %d，期望 %d", got, SchemaVersion)
	}
	for _, name := range []string{"shared_sessions", "shared_tasks", "shared_meta"} {
		if !tableExistsShared(t, st.DB(), name) {
			t.Errorf("新库缺少表 %s", name)
		}
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// 预检能读到刚写下的版本，且重复 Open 不报错（同版本幂等）。
	if exists, version, err := CheckPath(path); err != nil || !exists || version != SchemaVersion {
		t.Fatalf("CheckPath(新库) = (%v,%d,%v)，期望 (true,%d,nil)", exists, version, err, SchemaVersion)
	}
	again, err := Open(path, time.Now())
	if err != nil {
		t.Fatalf("同版本重复 Open 应允许，实际: %v", err)
	}
	_ = again.Close()
}

// TestSharedCheckPathDoesNotCreateFile 只读预检：不存在的路径返回 (false,0,nil)，
// 且绝不创建库文件；旧库预检也不改写字节。
func TestSharedCheckPathDoesNotCreateFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "absent.db")
	exists, version, err := CheckPath(path)
	if err != nil || exists || version != 0 {
		t.Fatalf("CheckPath(不存在) = (%v,%d,%v)，期望 (false,0,nil)", exists, version, err)
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("CheckPath 不应创建库文件: %v", statErr)
	}

	oldPath := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE memories (id TEXT PRIMARY KEY)`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	exists, version, err = CheckPath(oldPath)
	if err != nil || !exists || version != 0 {
		t.Fatalf("CheckPath(旧库) = (%v,%d,%v)，期望 (true,0,nil)", exists, version, err)
	}
	after, err := os.ReadFile(oldPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("CheckPath 改写了旧库文件（预检必须只读）")
	}
}

// ---------- 测试辅助（shared 包内没有现成的属性读取） ----------

func openSharedRawDB(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func pragmaVersion(t *testing.T, db *sql.DB) int {
	t.Helper()
	var v int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&v); err != nil {
		t.Fatalf("读 user_version: %v", err)
	}
	return v
}

func tableExistsShared(t *testing.T, db *sql.DB, name string) bool {
	t.Helper()
	ok, err := tableExists(db, name)
	if err != nil {
		t.Fatalf("查表 %s: %v", name, err)
	}
	return ok
}
