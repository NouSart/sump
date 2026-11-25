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
		Version: "stub/1", Methods: []spv2.Method{
			{Name: "work_open", Shape: spv2.ShapeCall},
			{Name: "store", Shape: spv2.ShapeCall},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	sm.srv = srv
	srv.Handle("work_open", func(_ context.Context, call *spv2.Call) error {
		sm.mu.Lock()
		sm.workOpens = append(sm.workOpens, memoryWorkOpen{FrameWork: call.Frame.Work})
		sm.mu.Unlock()
		return call.Reply(protocol.WorkOpenResult{TaskID: "t-stub"})
	})
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
	if broken.workOpensLen() < 2 {
		t.Fatalf("落库前应先登记 work，实际 work_open %d 次", broken.workOpensLen())
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

// TestQuietStoreOpensWorkThenStores 观察档的正常链路：先 work_open 登记会话级 work，
// 再带着同一个 work（帧头 + 载荷）与幂等键 store。work 按会话稳定，同会话的消息共用一个。
func TestQuietStoreOpensWorkThenStores(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "observe"}); !rp.OK {
		t.Fatalf("group.mode 应可热改: %+v", rp)
	}
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "随便聊聊", false))

	wantWork := "qq-quiet:qq:group:67890"
	opened := sm.waitWorkOpen(t, 5*time.Second)
	if opened.Payload.Work != wantWork || opened.FrameWork != wantWork {
		t.Fatalf("work_open 的帧头/载荷 work 应一致且按会话稳定: 帧头=%q 载荷=%q", opened.FrameWork, opened.Payload.Work)
	}
	if opened.Payload.ConversationID != "qq:group:67890" || opened.Payload.TaskName != "随便聊聊" || opened.Payload.Boss != "qq" {
		t.Fatalf("work_open 参数不符: %+v", opened.Payload)
	}

	saved := sm.waitStore(t, 5*time.Second)
	if saved.Payload.Work != wantWork || saved.FrameWork != wantWork {
		t.Fatalf("store 必须带同一个 work（帧头=%q 载荷=%q）", saved.FrameWork, saved.Payload.Work)
	}
	if saved.Payload.ClientItemID == "" || !strings.HasPrefix(saved.Payload.ClientItemID, "qq:") {
		t.Fatalf("store 应带 qq 生成的幂等键，实际 %q", saved.Payload.ClientItemID)
	}
	if saved.Payload.ConversationID != "qq:group:67890" || saved.Payload.Role != "user" || saved.Payload.Kind != "user" {
		t.Fatalf("store 其余字段应保持不变: %+v", saved.Payload)
	}
}

// TestQuietStoreSkipsStoreWhenWorkOpenFails 登记 work 失败就不发 store：
// 工作没登记，store 必然再失败一次；但仍然要留下可见信号（日志 + 累计次数）。
func TestQuietStoreSkipsStoreWhenWorkOpenFails(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	sm.mu.Lock()
	sm.failWorkOpen = true
	sm.mu.Unlock()
	var logs syncBuffer
	qq := startQQWithLog(t, sc, nc, "2271917353", io.MultiWriter(os.Stdout, &logs))
	nc.waitConn(t, 5*time.Second)

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "observe"}); !rp.OK {
		t.Fatalf("group.mode 应可热改: %+v", rp)
	}
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "登记不了", false))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && sm.workOpensLen() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if sm.workOpensLen() == 0 {
		t.Fatal("应当先尝试登记 work")
	}
	time.Sleep(300 * time.Millisecond)
	if got := sm.storesLen(); got != 0 {
		t.Fatalf("work_open 失败后不该再发 store，实际发了 %d 次", got)
	}
	text := logs.String()
	for _, want := range []string{"work_open", "累计 1 次", "这条消息不在上下文里了"} {
		if !strings.Contains(text, want) {
			t.Fatalf("work_open 失败要留下可见信号（缺 %q）:\n%s", want, text)
		}
	}
}
