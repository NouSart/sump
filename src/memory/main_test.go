package main

// 启动预检与显式重置的包内测试（S-001 §11）。
//
// 不通过 os/exec 起真实二进制（太慢、还依赖端口）：预检 / 重置 / 参数校验都抽成了
// 可直接调用的函数，这里逐条钉住关键性质——旧库被拒绝且**文件字节不变**（预检只读）、
// 新库 / 不存在放行、重置只改名不删除、备份目标冲突时不覆盖。

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

	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
)

// 说明：测试里 sql.Open("sqlite", ...) 用的驱动由 store 包的 blank import 注册
// （memory 本来就依赖 store），这里不额外引入第三方依赖。

// newV1MemoryDB 用 store.Open 生成一个当前版本（user_version=SchemaVersion）的会话库。
func newV1MemoryDB(t *testing.T, path string) {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("生成新版本会话库 %s 失败: %v", path, err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("关闭会话库 %s 失败: %v", path, err)
	}
}

// newSharedDB 生成一个真实的共享库文件（预检只 os.Stat 它，不读版本）。
func newSharedDB(t *testing.T, path string) {
	t.Helper()
	sh, err := shared.Open(path, time.Now())
	if err != nil {
		t.Fatalf("生成共享库 %s 失败: %v", path, err)
	}
	if err := sh.Close(); err != nil {
		t.Fatalf("关闭共享库 %s 失败: %v", path, err)
	}
}

// newLegacyDB 用 raw sqlite 造旧库：user_version 保持 0，只有一张表（旧版 memory.db 的形状）。
func newLegacyDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开旧库 %s 失败: %v", path, err)
	}
	if _, err := db.Exec(`CREATE TABLE works (id TEXT PRIMARY KEY)`); err != nil {
		_ = db.Close()
		t.Fatalf("造旧库 %s 失败: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭旧库 %s 失败: %v", path, err)
	}
}

// newFutureDB 造一个版本号高于期望的库（回退启动 / 降级安装的场景）。
func newFutureDB(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("打开高版本库 %s 失败: %v", path, err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (x TEXT); PRAGMA user_version = 99`); err != nil {
		_ = db.Close()
		t.Fatalf("造高版本库 %s 失败: %v", path, err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("关闭高版本库 %s 失败: %v", path, err)
	}
}

// TestPrecheckRejectsLegacyDatabase 旧库（user_version=0 且已有表）必须拒绝启动：
// 错误文本含当前版本、期望版本、两个库路径与两条可选操作；预检只读，库文件字节不变。
func TestPrecheckRejectsLegacyDatabase(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")
	newLegacyDB(t, memoryPath)

	before, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatalf("读取旧库字节失败: %v", err)
	}

	err = precheck(memoryPath, sharedPath)
	if err == nil {
		t.Fatal("旧库应被预检拒绝")
	}
	for _, want := range []string{
		memoryPath,
		sharedPath,
		"当前版本 0",
		fmt.Sprintf("期望版本 %d", store.SchemaVersion),
		"-reset-data",
		"-reset-data-confirm=YES",
		"删除",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("预检错误缺少 %q:\n%s", want, err)
		}
	}

	// 预检必须只读：库文件一个字节都不能变，也不能顺手把共享库建出来。
	after, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatalf("读取旧库字节失败: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("预检改写了旧库文件（DDL 不能发生在版本闸门之前）")
	}
	if _, statErr := os.Stat(sharedPath); !errors.Is(statErr, os.ErrNotExist) {
		t.Errorf("预检不应创建共享库文件: %v", statErr)
	}
}

// TestPrecheckRejectsFutureVersion 版本高于期望（旧程序打开新库）：同样拒绝，且不动库。
func TestPrecheckRejectsFutureVersion(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")
	newFutureDB(t, memoryPath)

	before, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatalf("读取高版本库字节失败: %v", err)
	}
	err = precheck(memoryPath, sharedPath)
	if err == nil {
		t.Fatal("高版本库应被预检拒绝")
	}
	if !strings.Contains(err.Error(), "当前版本 99") ||
		!strings.Contains(err.Error(), fmt.Sprintf("期望版本 %d", store.SchemaVersion)) {
		t.Fatalf("预检错误应含两个版本号:\n%s", err)
	}
	after, err := os.ReadFile(memoryPath)
	if err != nil {
		t.Fatalf("读取高版本库字节失败: %v", err)
	}
	if !bytes.Equal(before, after) {
		t.Error("预检改写了高版本库文件")
	}
}

// TestPrecheckAcceptsNewDatabase 新库（user_version == store.SchemaVersion）放行。
func TestPrecheckAcceptsNewDatabase(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")
	newV1MemoryDB(t, memoryPath)
	newSharedDB(t, sharedPath)

	if err := precheck(memoryPath, sharedPath); err != nil {
		t.Fatalf("新库应通过预检，实际失败: %v", err)
	}
}

// TestPrecheckAcceptsMissingDatabase 两个库都不存在 = 新库，放行；预检不得建任何文件。
func TestPrecheckAcceptsMissingDatabase(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")

	if err := precheck(memoryPath, sharedPath); err != nil {
		t.Fatalf("不存在的库应通过预检，实际失败: %v", err)
	}
	for _, path := range []string{memoryPath, sharedPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("预检不应创建 %s: %v", path, err)
		}
	}
}

// TestResetWithBackup 显式重置：两个库改名为 *.bak-<UTC 时间戳>，原文消失、备份内容不变；
// 重置后按新库预检（两个路径都已不存在）。
func TestResetWithBackup(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")
	newV1MemoryDB(t, memoryPath)
	newSharedDB(t, sharedPath)

	before := make(map[string][]byte, 2)
	for _, path := range []string{memoryPath, sharedPath} {
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("读取 %s 字节失败: %v", path, err)
		}
		before[path] = b
	}

	// 固定时间，方便断言备份文件名（UTC，避免时区影响）。
	now := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	if err := resetWithBackup(memoryPath, sharedPath, now); err != nil {
		t.Fatalf("重置失败: %v", err)
	}

	const suffix = ".bak-20261124-093000"
	for _, path := range []string{memoryPath, sharedPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("重置后原文件应消失: %s（%v）", path, err)
		}
		bak := path + suffix
		after, err := os.ReadFile(bak)
		if err != nil {
			t.Errorf("重置后应存在备份 %s: %v", bak, err)
			continue
		}
		if !bytes.Equal(before[path], after) {
			t.Errorf("备份 %s 的内容与原文件不一致", bak)
		}
	}

	// 两个路径都不存在 = 新库，预检放行，可以按新库启动。
	if err := precheck(memoryPath, sharedPath); err != nil {
		t.Fatalf("重置后预检应放行，实际失败: %v", err)
	}
}

// TestResetWithBackupMissingFiles 文件本来不存在（全新安装）时重置不算错，直接按新库启动。
func TestResetWithBackupMissingFiles(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")

	if err := resetWithBackup(memoryPath, sharedPath, time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("文件不存在时重置应成功，实际失败: %v", err)
	}
}

// TestResetWithBackupRefusesExistingTarget 备份目标已存在：报错且**不覆盖**，
// 也不把任何原文件改名（先检查全部目标再动手）。
func TestResetWithBackupRefusesExistingTarget(t *testing.T) {
	dir := t.TempDir()
	memoryPath := filepath.Join(dir, "memory.db")
	sharedPath := filepath.Join(dir, "experience.db")
	newV1MemoryDB(t, memoryPath)
	newSharedDB(t, sharedPath)

	now := time.Date(2026, 11, 24, 9, 30, 0, 0, time.UTC)
	target := memoryPath + ".bak-20261124-093000"
	const existing = "已有备份，不许覆盖"
	if err := os.WriteFile(target, []byte(existing), 0o600); err != nil {
		t.Fatalf("预置备份目标失败: %v", err)
	}

	err := resetWithBackup(memoryPath, sharedPath, now)
	if err == nil {
		t.Fatal("备份目标已存在时应报错，且不得覆盖")
	}
	if !strings.Contains(err.Error(), target) || !strings.Contains(err.Error(), "已存在") {
		t.Errorf("错误应指明冲突的备份目标:\n%s", err)
	}

	// 原文件都还在（两阶段检查：冲突时一个都不改名）。
	for _, path := range []string{memoryPath, sharedPath} {
		if _, statErr := os.Stat(path); statErr != nil {
			t.Errorf("备份目标冲突时原文件不应被改名 %s: %v", path, statErr)
		}
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("读取备份目标失败: %v", err)
	}
	if string(got) != existing {
		t.Error("已存在的备份被覆盖了")
	}
}

// TestCheckResetFlags 二次确认是重置的唯一入口：只有 -reset-data + YES 生效；
// 确认串不对、或只给确认串，都必须报错（不能静默忽略）。
func TestCheckResetFlags(t *testing.T) {
	cases := []struct {
		name    string
		reset   bool
		confirm string
		wantErr bool
	}{
		{"都没给", false, "", false},
		{"只给确认串", false, "YES", true},
		{"重置但没确认串", true, "", true},
		{"确认串不对", true, "yes", true},
		{"确认串是 NO", true, "NO", true},
		{"重置且确认串正确", true, "YES", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkResetFlags(tc.reset, tc.confirm)
			if tc.wantErr && err == nil {
				t.Fatalf("reset=%v confirm=%q 应报错", tc.reset, tc.confirm)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("reset=%v confirm=%q 不应报错: %v", tc.reset, tc.confirm, err)
			}
		})
	}
}
