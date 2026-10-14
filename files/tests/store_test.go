package tests

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/files/store"
)

// TestStoreSaveLoadStat 验证落盘/元数据/Stat/读取与不存在场景。
func TestStoreSaveLoadStat(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	data := []byte("# 你好\n文件内容")
	id, err := st.Save(data, "notes.md", "text/markdown")
	if err != nil {
		t.Fatal(err)
	}
	if id == "" {
		t.Fatal("id 为空")
	}
	meta, err := st.Stat(id)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Filename != "notes.md" || meta.Mime != "text/markdown" || meta.Size != int64(len(data)) {
		t.Fatalf("Stat 不符: %+v", meta)
	}
	got, m2, err := st.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) || m2.Filename != "notes.md" {
		t.Fatalf("读取不符: len=%d meta=%+v", len(got), m2)
	}
	if _, err := st.Stat("不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Stat 期望 ErrNotFound，得到: %v", err)
	}
	if _, _, err := st.Load("不存在"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("Load 期望 ErrNotFound，得到: %v", err)
	}
}
