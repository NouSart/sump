package protocol_test

// NewID 的并发契约：唯一、同毫秒内递增、可并发调用。
//
// 这条用例是被 `-race` 逼出来的：`ulid.Monotonic` 内部用 `bufio.Reader` 包熵源，
// 而 bufio 的读不能并发——两个 goroutine 同时 NewID 就会在缓冲里撞上（实测被竞态检测抓到）。
// 现在外面加了一把锁。普通 `go test` 看不出问题，所以要靠 `-race` 和"唯一性"两处兜住。

import (
	"sync"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

func TestNewIDConcurrentUnique(t *testing.T) {
	const n = 2000
	ids := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i] = protocol.NewID()
		}(i)
	}
	close(start)
	wg.Wait()

	seen := make(map[string]bool, n)
	for i, id := range ids {
		if len(id) != 26 {
			t.Fatalf("第 %d 个 id 长度不对: %q", i, id)
		}
		if seen[id] {
			t.Fatalf("并发生成的 id 撞了: %q", id)
		}
		seen[id] = true
	}
}

// TestNewIDMonotonic 验证同毫秒内仍单调递增（这把锁没有把"单调"这个性质弄丢）。
func TestNewIDMonotonic(t *testing.T) {
	prev := protocol.NewID()
	for i := 0; i < 500; i++ {
		cur := protocol.NewID()
		if cur <= prev {
			t.Fatalf("同一毫秒内应单调递增: %q -> %q", prev, cur)
		}
		prev = cur
	}
}
