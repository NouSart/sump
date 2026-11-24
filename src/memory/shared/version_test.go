package shared

// 共享库版本闸门的包内测试（S-001 §11.1 / D3「不迁移、重置」）。
//
// 为什么要在本包再测一次：main 那边测的是「启动流程会不会拒绝」，
// 真正的判定在 Open 里——闸门一旦被改掉，旧库会被 CREATE TABLE IF NOT EXISTS
// 悄悄补出新列，共享库的表结构承诺就不成立了。异变检测只跑被改文件所在包的测试，
// 所以这条必须在 shared 包内成立。

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// newLegacyFile 造一个「有表但没有版本号」的库，模拟旧版共享经验库。
func newLegacyFile(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开旧共享库 %s 失败: %v", path, err)
	}
	if _, err := db.Exec(`CREATE TABLE shared_sessions (id TEXT PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("造旧共享库 %s 失败: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭旧共享库 %s 失败: %v", path, err)
	}
}

// TestOpenRefusesLegacyLibrary user_version=0 且已有表 = 旧库：Open 必须失败，
// 且失败发生在任何 DDL 之前——库文件字节必须一模一样。
func TestOpenRefusesLegacyLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experience.db")
	newLegacyFile(t, path)

	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := Open(path, time.Now())
	if err == nil {
		_ = st.Close()
		t.Fatal("旧共享库应被 Open 拒绝")
	}
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("应报 ErrSchemaVersionMismatch，实际: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Error("Open 在版本闸门之前就改写了库文件")
	}
}

// TestOpenRefusesHigherVersionLibrary 比本程序更新的库（version = SchemaVersion+1）同样拒绝：
// 旧进程读新格式不可信，不能靠 IF NOT EXISTS 蒙混过去。
func TestOpenRefusesHigherVersionLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experience.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion+1)); err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	st, err := Open(path, time.Now())
	if err == nil {
		_ = st.Close()
		t.Fatal("更高版本的共享库应被 Open 拒绝")
	}
	if !errors.Is(err, ErrSchemaVersionMismatch) {
		t.Fatalf("应报 ErrSchemaVersionMismatch，实际: %v", err)
	}
}

// TestOpenAllowsFreshLibrary 空库（文件不存在 / version=0 且无表）放行，
// 且建表后必须把版本号写进去，否则第二次启动会被自己的闸门锁死。
func TestOpenAllowsFreshLibrary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "experience.db")
	st, err := Open(path, time.Now())
	if err != nil {
		t.Fatalf("新库应放行: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	exists, version, err := CheckPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if !exists || version != SchemaVersion {
		t.Fatalf("Open 应把共享库版本写为 %d，实际 exists=%v version=%d", SchemaVersion, exists, version)
	}
	st2, err := Open(path, time.Now())
	if err != nil {
		t.Fatalf("同版本二次启动应放行: %v", err)
	}
	if err := st2.Close(); err != nil {
		t.Fatal(err)
	}
}

// TestCheckPathNeverCreatesFile 预检必须只读：给定不存在的路径后磁盘上不能多出文件。
func TestCheckPathNeverCreatesFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-created.db")
	exists, version, err := CheckPath(path)
	if err != nil {
		t.Fatal(err)
	}
	if exists || version != 0 {
		t.Fatalf("不存在的路径应返回 (false, 0)，实际 (%v, %d)", exists, version)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("预检创建了库文件: %v", err)
	}
}
