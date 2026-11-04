// store 一致性的根因实验（先证明缺陷，再修复）。
//
// 契约：
//   - Overwrite 返回错误时，调用方会认为文件仍是原内容；权威副本与元数据必须一起回滚。
//   - Load 返回的二进制与 Meta.Size 必须来自同一个快照，不能出现"新内容 + 旧尺寸"。
package store

import (
	"bytes"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// 模拟元数据更新失败：文件已经写下去了，但 UPDATE 被 SQLite 触发器拒绝。
// 若实现没有回滚，Load 会读到新字节 + 旧尺寸——调用方收到错误却已丢失原文。
func TestOverwriteRollsBackWhenMetadataUpdateFails(t *testing.T) {
	s := openTemp(t)

	original := []byte("原始内容")
	id, err := s.Save(original, "a.txt", "text/plain")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.db.Exec(`
CREATE TRIGGER fail_files_update
BEFORE UPDATE ON files
BEGIN
  SELECT RAISE(ABORT, 'injected update failure');
END;`); err != nil {
		t.Fatalf("创建故障触发器失败（实验无效）: %v", err)
	}

	updated := bytes.Repeat([]byte("新内容"), 64)
	err = s.Overwrite(id, updated)
	if err == nil {
		t.Fatal("触发器注入后 Overwrite 仍成功（实验无效）")
	}
	if !strings.Contains(err.Error(), "injected update failure") {
		t.Fatalf("错误不是预期的注入失败（实验无效）: %v", err)
	}

	got, meta, loadErr := s.Load(id)
	if loadErr != nil {
		t.Fatalf("失败后 Load: %v", loadErr)
	}
	if !bytes.Equal(got, original) {
		t.Fatalf("Overwrite 报错后文件已被改写：原 %d 字节，现 %d 字节", len(original), len(got))
	}
	if meta.Size != int64(len(original)) {
		t.Fatalf("Overwrite 报错后元数据尺寸被改：原 %d，现 %d", len(original), meta.Size)
	}
}

// 并发读写时，每次 Load 都必须是完整快照：
// 数据要么全 A 要么全 B，且 size 与本次读到的字节数一致。
func TestLoadIsSnapshotDuringConcurrentOverwrite(t *testing.T) {
	s := openTemp(t)

	a := bytes.Repeat([]byte("A"), 512<<10)
	b := bytes.Repeat([]byte("B"), 768<<10)
	id, err := s.Save(a, "race.bin", "application/octet-stream")
	if err != nil {
		t.Fatal(err)
	}

	const rounds = 80
	var stop atomic.Bool
	var wg sync.WaitGroup
	errCh := make(chan string, 1)

	report := func(msg string) {
		select {
		case errCh <- msg:
		default:
		}
	}

	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for !stop.Load() {
				got, meta, err := s.Load(id)
				if err != nil {
					report("并发 Load 失败: " + err.Error())
					return
				}
				if !bytes.Equal(got, a) && !bytes.Equal(got, b) {
					report("Load 读到混合或截断内容: size=" + strconv.Itoa(len(got)))
					return
				}
				if meta.Size != int64(len(got)) {
					report("Load 快照不一致: size=" + strconv.Itoa(int(meta.Size)) + " data=" + strconv.Itoa(len(got)))
					return
				}
			}
		}()
	}

	start := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-start
		for i := 0; i < rounds; i++ {
			data := a
			if i%2 == 1 {
				data = b
			}
			if err := s.Overwrite(id, data); err != nil {
				report("并发 Overwrite 失败: " + err.Error())
				break
			}
		}
		stop.Store(true)
	}()

	close(start)
	wg.Wait()
	close(errCh)
	if msg, ok := <-errCh; ok {
		t.Fatal(msg)
	}
}
