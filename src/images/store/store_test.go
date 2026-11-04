// images/store 的直接测试。
//
// 之前只有 images/tests 里的单张图片用例：每个用例都新开 store，永远存不到第二张，
// 于是「固定文件名覆盖」「size 记错」「INSERT 失败留孤儿」「连接池放开」都能存活。
package store

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func openTemp(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "meta", "images.db"), filepath.Join(dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// 两张不同图片必须各自落盘，按 id 取回自己的字节。
func TestSaveTwoImagesDoNotOverwrite(t *testing.T) {
	s := openTemp(t)

	first := []byte("第一张图片的字节")
	second := []byte("第二张图片，长度与内容都不同")
	id1, err := s.Save(first, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	id2, err := s.Save(second, "image/jpeg")
	if err != nil {
		t.Fatal(err)
	}
	if id1 == id2 {
		t.Fatal("两次保存的 id 不该相同")
	}

	got1, mime1, err := s.Load(id1)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got1, first) || mime1 != "image/png" {
		t.Fatalf("第一张被覆盖或 mime 不符: data=%q mime=%q", got1, mime1)
	}

	got2, mime2, err := s.Load(id2)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got2, second) || mime2 != "image/jpeg" {
		t.Fatalf("第二张不符: data=%q mime=%q", got2, mime2)
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("应有两个独立二进制文件，实际 %d 个", len(entries))
	}
}

// 台账里的 size 与磁盘实际字节数必须一致。
func TestSaveRecordsAccurateSize(t *testing.T) {
	s := openTemp(t)

	data := bytes.Repeat([]byte("图"), 37)
	id, err := s.Save(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}

	var size int64
	var path string
	if err := s.db.QueryRow(`SELECT size, path FROM images WHERE id=?`, id).Scan(&size, &path); err != nil {
		t.Fatal(err)
	}
	if size != int64(len(data)) {
		t.Fatalf("台账 size 应为 %d，实际 %d", len(data), size)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Size() != int64(len(data)) {
		t.Fatalf("磁盘文件应为 %d 字节，实际 %d", len(data), st.Size())
	}
}

// 落库失败时必须删掉已经写下的二进制，不能留孤儿。
func TestSaveRemovesOrphanWhenInsertFails(t *testing.T) {
	s := openTemp(t)

	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Save([]byte("x"), "image/png"); err == nil {
		t.Fatal("库已关闭时 Save 应报错")
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("落库失败后不该留下二进制文件，实际 %v", entries)
	}
}

// Open 必须自己创建 db 的父目录和二进制目录，调用方不需要先 MkdirAll。
func TestOpenCreatesNestedParents(t *testing.T) {
	base := t.TempDir()
	s, err := Open(
		filepath.Join(base, "a", "b", "meta.db"),
		filepath.Join(base, "x", "y", "blobs"),
	)
	if err != nil {
		t.Fatalf("嵌套目录未创建: %v", err)
	}
	defer s.Close()

	data := []byte("嵌套目录图片")
	id, err := s.Save(data, "image/png")
	if err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Load(id)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("读取不符: %q", got)
	}
}

// 并发保存必须各自成功、各自可读，且保持 SQLite 单写者连接池。
func TestConcurrentSavesKeepEveryImage(t *testing.T) {
	s := openTemp(t)

	const n = 24
	ids := make([]string, n)
	bodies := make([][]byte, n)
	errCh := make(chan string, n)

	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			bodies[i] = []byte(fmt.Sprintf("并发图片-%02d", i))
			id, err := s.Save(bodies[i], "image/png")
			if err != nil {
				errCh <- fmt.Sprintf("第 %d 个 Save 失败: %v", i, err)
				return
			}
			ids[i] = id
		}(i)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}

	if got := s.db.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("SQLite 连接池应为 1（单写者），实际 %d", got)
	}

	seen := make(map[string]bool, n)
	for i, id := range ids {
		if id == "" {
			t.Fatalf("第 %d 个 Save 没有返回 id", i)
		}
		if seen[id] {
			t.Fatalf("第 %d 个 Save 返回了重复 id %q", i, id)
		}
		seen[id] = true
		got, _, err := s.Load(id)
		if err != nil {
			t.Fatalf("Load(%q) 失败: %v", id, err)
		}
		if !bytes.Equal(got, bodies[i]) {
			t.Fatalf("图片 %d 被别的写入覆盖: 想要 %q，实际 %q", i, bodies[i], got)
		}
	}

	entries, err := os.ReadDir(s.dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != n {
		t.Fatalf("并发保存后应有 %d 个二进制文件，实际 %d 个", n, len(entries))
	}
}
