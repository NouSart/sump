package tests

// "只记录不推理"这条路径落库失败时，必须有**看得见的信号**。
//
// 为什么单独一条：这条消息不进推理链，落库失败就是**彻底消失**——上下文里没有它，门户里也
// 没有它。只写一行"失败了"的日志，没人知道漏了多少条。现在失败会带累计次数（日志里能看出规模），
// 并且沿既有的 report 通道上报设置中心（与"跳转未送达"同一处可见）。

import (
	"bytes"
	"context"
	"io"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// startBrokenMemory 起一个"存不进去"的记忆替身：store 一律回失败。
func startBrokenMemory(t *testing.T, center *stubCenter) *stubMemory {
	t.Helper()
	sm := &stubMemory{}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "memory", Actor: spv2.ServiceRef("memory"),
		Version: "stub/1", Methods: []spv2.Method{{Name: "store", Shape: spv2.ShapeCall}},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	sm.srv = srv
	srv.Handle("store", func(_ context.Context, call *spv2.Call) error {
		sm.mu.Lock()
		sm.stores = append(sm.stores, memoryStore{})
		sm.mu.Unlock()
		return call.ReplyError(spv2.CodeInternal, "记忆库写不进去", false, nil)
	})
	sm.http = httptest.NewServer(srv.Handler())
	t.Cleanup(sm.http.Close)
	registerService(t, center, "memory", sm.URL())
	return sm
}

// syncBuffer 是并发安全的日志缓冲（qq 在多个 goroutine 里写日志）。
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// TestQuietStoreFailureIsVisible 观察档下落库失败：不静默——日志带累计次数，且消息确实没进推理链。
func TestQuietStoreFailureIsVisible(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	broken := startBrokenMemory(t, sc)
	var logs syncBuffer
	qq := startQQWithLog(t, sc, nc, "2271917353", io.MultiWriter(os.Stdout, &logs))
	_ = al
	nc.waitConn(t, 5*time.Second)

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "observe"}); !rp.OK {
		t.Fatalf("group.mode 应可热改: %+v", rp)
	}
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第一句", false))
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第二句", false))

	// 两次都真的打到了记忆服务（证明失败是"落了但没成功"，不是压根没调）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && broken.storesLen() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if broken.storesLen() < 2 {
		t.Fatalf("应当尝试落库两条，实际 %d", broken.storesLen())
	}

	// 日志里要看得见"累计"——这是"漏了多少条"的唯一线索。
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(logs.String(), "累计 2 次") {
		time.Sleep(20 * time.Millisecond)
	}
	text := logs.String()
	if !strings.Contains(text, "累计 1 次") || !strings.Contains(text, "累计 2 次") {
		t.Fatalf("落库失败要带累计次数（否则没人知道漏了多少）:\n%s", text)
	}
	if !strings.Contains(text, "这条消息不在上下文里了") {
		t.Fatalf("日志要把后果说清楚（消息消失），而不是只报一个错误:\n%s", text)
	}
	// 观察档的消息不进推理链：失败也不该突然去打扰模型。
	if al.count() != 0 {
		t.Fatalf("观察档不该触发推理，实际 %d 条", al.count())
	}
}
