package runner

import (
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 跨进程编译锁必须真的串行化：两个评测测试进程同时 build 同一个服务时，
// 不能让它们同时进入「查缓存 → 编译 → 写 stamp」这一段。
func TestBuildLockSerializes(t *testing.T) {
	dir := t.TempDir()
	var active, maxActive int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := withBuildLock(dir, "svc", func() error {
				n := atomic.AddInt32(&active, 1)
				defer atomic.AddInt32(&active, -1)
				for {
					m := atomic.LoadInt32(&maxActive)
					if n <= m || atomic.CompareAndSwapInt32(&maxActive, m, n) {
						break
					}
				}
				time.Sleep(20 * time.Millisecond)
				return nil
			})
			if err != nil {
				t.Errorf("withBuildLock: %v", err)
			}
		}()
	}
	wg.Wait()
	if maxActive != 1 {
		t.Fatalf("并发进入临界区的最大值 = %d, want 1", maxActive)
	}
	if _, err := os.Stat(filepath.Join(dir, ".svc.lock")); !os.IsNotExist(err) {
		t.Fatalf("锁文件没有清理: %v", err)
	}
}

func TestBuildLockReclaimsStale(t *testing.T) {
	dir := t.TempDir()
	lock := filepath.Join(dir, ".svc.lock")
	if err := os.WriteFile(lock, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-10 * time.Minute)
	if err := os.Chtimes(lock, old, old); err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := withBuildLock(dir, "svc", func() error { ran = true; return nil }); err != nil {
		t.Fatalf("崩溃遗留锁应被回收: %v", err)
	}
	if !ran {
		t.Fatal("回收旧锁后没有执行构建函数")
	}
}
