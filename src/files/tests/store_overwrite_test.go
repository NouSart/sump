package tests

// files/store 的落盘契约：**id 一证三用**（数据库主键 / 磁盘文件名 / 链上引用）。
//
// 这几条都"改坏了也不会报错"——文件照样能读，只是内容或尺寸不对，或者两个同名文件互相覆盖。
// 之前的 store 用例只存过一个文件，等于把最危险的那一半留在了测试之外：
//   - 落盘路径用调用方给的 filename → 路径穿越 + 同名覆盖；
//   - Overwrite 不更新 size → 配额判定（max_mb）基于旧尺寸；
//   - Overwrite 缺件当成成功 → "覆盖了不存在的文件"没人发现。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/NouSart/sump/files/store"
)

func openStore(t *testing.T) (*store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(t.TempDir(), "files.db"), filepath.Join(dir, "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, filepath.Join(dir, "files")
}

// TestStoreTwoFilesIndependent 同名文件存两遍也必须互不覆盖，且各自按 id 取回自己的内容。
func TestStoreTwoFilesIndependent(t *testing.T) {
	st, dir := openStore(t)

	first := []byte("第一份")
	second := []byte("第二份（更长一点）")
	id1, err := st.Save(first, "同名.md", "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := st.Save(second, "同名.md", "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("两次保存必须得到不同的 id（同名文件不是同一个东西）")
	}

	got1, meta1, err := st.Load(id1)
	if err != nil {
		t.Fatal(err)
	}
	got2, meta2, err := st.Load(id2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, first) || !bytes.Equal(got2, second) {
		t.Fatalf("两份内容互相污染了: %q / %q", got1, got2)
	}
	if meta1.Size != int64(len(first)) || meta2.Size != int64(len(second)) {
		t.Fatalf("尺寸台账不符: %d / %d", meta1.Size, meta2.Size)
	}

	// 落盘文件名必须是 id：用 filename 的话，两次保存会写到同一个路径（后写覆盖先写）。
	for _, id := range []string{id1, id2} {
		if _, err := os.Stat(filepath.Join(dir, id)); err != nil {
			t.Fatalf("磁盘上找不到以 id 命名的文件 %s: %v", id, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "同名.md")); err == nil {
		t.Fatal("不该用调用方给的 filename 落盘（同名文件会互相覆盖，且能写出目录）")
	}
}

// TestStoreOverwriteUpdatesSizeAndBytes 覆盖后尺寸与内容一起变——配额判定看的是台账。
func TestStoreOverwriteUpdatesSizeAndBytes(t *testing.T) {
	st, _ := openStore(t)

	id, err := st.Save([]byte("短"), "a.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}
	longer := []byte("覆盖之后的更长内容")
	if err := st.Overwrite(id, longer); err != nil {
		t.Fatal(err)
	}

	meta, err := st.Stat(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Size != int64(len(longer)) {
		t.Fatalf("覆盖后 size 必须跟着变（否则 max_mb 配额按旧尺寸算）: %d, want %d", meta.Size, len(longer))
	}
	got, loaded, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, longer) || loaded.Size != int64(len(longer)) {
		t.Fatalf("覆盖后读到的内容/尺寸不符: %q size=%d", got, loaded.Size)
	}
	// 文件名不变（就地改内容，不是另存一份）。
	if meta.Filename != "a.txt" || meta.Mime != "text/plain" {
		t.Fatalf("覆盖不该改动文件名 / 类型: %+v", meta)
	}
}

// TestStoreOverwriteMissingIsNotFound 覆盖一个不存在的 id 必须报 ErrNotFound：
// 当成成功会让上层以为"改好了"，而磁盘上什么都没有。
func TestStoreOverwriteMissingIsNotFound(t *testing.T) {
	st, _ := openStore(t)
	if err := st.Overwrite("不存在的-id", []byte("x")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，得到: %v", err)
	}
}
