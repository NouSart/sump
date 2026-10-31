// store 层的直接测试。
//
// 之前 `files/store` 没有任何包内测试，唯一涉及它的用例（files/tests 的
// TestStoreSaveLoadStat）只存一个文件 —— 于是这些改动都能安全地「通过」：
//   - Save 的落盘路径改用调用方给的 filename（ULID 一证三用的前提被抽掉）
//   - Overwrite 不更新 size / 缺件返回 nil
package store

import (
	"bytes"
	"path/filepath"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "meta", "files.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 存两个文件：各自能按 id 取回原内容，互不覆盖（钉死「id 一证三用」）。
func TestSaveTwoFilesDoNotOverwrite(t *testing.T) {
	s := openTemp(t)

	first := []byte("第一份内容")
	second := []byte("第二份内容，长度不同")

	id1, err := s.Save(first, "a.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Save(second, "b.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("两次保存的 id 不该相同")
	}

	got1, meta1, err := s.Load(id1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, first) || meta1.Filename != "a.txt" || meta1.Size != int64(len(first)) {
		t.Fatalf("第一份被覆盖了: data=%q meta=%+v", got1, meta1)
	}

	got2, meta2, err := s.Load(id2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, second) || meta2.Filename != "b.txt" || meta2.Size != int64(len(second)) {
		t.Fatalf("第二份不符: data=%q meta=%+v", got2, meta2)
	}
}

// Overwrite 必须把新尺寸写回台账：配额判定（max_mb）读的就是这个值。
func TestOverwriteUpdatesSize(t *testing.T) {
	s := openTemp(t)

	id, err := s.Save([]byte("短"), "a.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	long := bytes.Repeat([]byte("长"), 100)
	if err := s.Overwrite(id, long); err != nil {
		t.Fatal(err)
	}

	meta, err := s.Stat(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(long)) {
		t.Fatalf("Overwrite 后尺寸应是 %d，实际 %d（配额会按旧值算）", len(long), meta.Size)
	}
	got, _, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, long) {
		t.Fatalf("内容没写进去: %d 字节", len(got))
	}
}

// 同一个 filename 存两次：两个 id 各自保留自己的字节。
// 落盘名必须是 id（ULID 一证三用），不能用调用方给的 filename —— 否则同名即互相覆盖。
func TestSaveSameFilenameKeepsBothBlobs(t *testing.T) {
	s := openTemp(t)

	first := []byte("第一次的同名文件")
	second := []byte("第二次的同名文件（更长一些）")

	id1, err := s.Save(first, "同名.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Save(second, "同名.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	got1, _, err := s.Load(id1)
	if err != nil {
		t.Fatal(err)
	}
	got2, _, err := s.Load(id2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, first) {
		t.Fatalf("同名第二次保存把第一次覆盖了: %q", got1)
	}
	if !bytes.Equal(got2, second) {
		t.Fatalf("第二次内容不符: %q", got2)
	}
}

// 覆盖一个不存在的 id 必须报 ErrNotFound，不能当成功。
func TestOverwriteMissingReturnsNotFound(t *testing.T) {
	s := openTemp(t)
	if err := s.Overwrite("no-such-id", []byte("x")); err != ErrNotFound {
		t.Fatalf("应返回 ErrNotFound，实际 %v", err)
	}
}

// 读不存在的 id：ErrNotFound，不 panic。
func TestLoadMissingReturnsNotFound(t *testing.T) {
	s := openTemp(t)
	if _, _, err := s.Load("no-such-id"); err != ErrNotFound {
		t.Fatalf("Load 应返回 ErrNotFound，实际 %v", err)
	}
	if _, err := s.Stat("no-such-id"); err != ErrNotFound {
		t.Fatalf("Stat 应返回 ErrNotFound，实际 %v", err)
	}
}

// 落库失败时不能留孤儿二进制文件（Save 写完盘才 INSERT，失败要回滚）。
func TestSaveRemovesOrphanWhenInsertFails(t *testing.T) {
	s := openTemp(t)

	// 关掉库 → INSERT 必然失败。
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save([]byte("x"), "a.txt", "text/plain"); err == nil {
		t.Fatal("库已关闭时 Save 应报错")
	}

	entries, err := filepath.Glob(filepath.Join(s.dir, "*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("落库失败后不该留下二进制文件，实际 %v", entries)
	}
}
