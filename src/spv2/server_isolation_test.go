package spv2

// 服务端两条隔离承诺（docs/concepts/spv2.md 附录 A 第 1 条「无静默终态：call 必须有
// reply/stream end；event 不是终态；drop 必计数」，验收口径见同文 §17.2「掩耳盗铃」一行）：
//
//   1. 处理方 panic 不外溢：调用方拿到 internal 回执（不是空等），进程继续活着，panic 记 sump_panics_total；
//   2. 被丢弃的入站帧一律计数 + 落日志，且**一类事件一套记账**——call / event 校验失败走同一条指标序列，
//      不为「非 call 帧」另立第二个计数器。
//
// 只用本机 httptest 与 127.0.0.1 的 WS，不访问外网；等待用指标轮询 / 读超时，不长 sleep。

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// lockedBuffer 是并发安全的日志缓冲：panic / 丢弃日志来自不同 goroutine。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// reasonCounter 取 sump_dropped_total 某个 reason 的值。
func reasonCounter(ms []Metric, name, reason string) float64 {
	for _, m := range ms {
		if m.Name == name && m.Labels["reason"] == reason {
			return m.Value
		}
	}
	return 0
}

// waitReasonCounter 轮询等待某个计数到位（服务端在 dispatch 里记账，可能晚于线上回执）。
func waitReasonCounter(t *testing.T, srv *Server, name, reason string, want float64) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	var got float64
	for {
		got = reasonCounter(srv.Metrics().Snapshot(), name, reason)
		if got >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s{reason=%q} = %v, want >= %v", name, reason, got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

// wsWriteFrame 用裸连接发一帧（绕过 client 的本地校验，让服务端自己那一半行为可观测）。
func wsWriteFrame(t *testing.T, conn *websocket.Conn, f Frame) {
	t.Helper()
	data, err := Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

// wsExpectNoFrame 确认一段时间内线上没有第二帧。
func wsExpectNoFrame(t *testing.T, conn *websocket.Conn, within time.Duration) {
	t.Helper()
	if err := conn.SetReadDeadline(time.Now().Add(within)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, extra, err := conn.ReadMessage(); err == nil {
		t.Fatalf("多出一帧: %q", extra)
	}
}

// TestServerHandlerPanicRepliesInternal 钉住 panic 隔离：处理方炸了，调用方必须收到
// code=internal 的错误回执而不是等到超时，panic 记进 sump_panics_total，
// 同一条连接随后还能正常用（进程没被这次 panic 带走）。
func TestServerHandlerPanicRepliesInternal(t *testing.T) {
	var logged lockedBuffer
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("boom"), wsTestMethod("echo")), WithLogger(log.New(&logged, "", 0)))
	srv.Handle("boom", func(context.Context, *Call) error { panic("处理器炸了") })
	srv.Handle("echo", func(_ context.Context, c *Call) error { return c.Reply("ok") })

	c := wsDialClient(t, url)
	_, err := c.Call(wsCtx(t), wsCall(t, AgentRef("client"), "boom", nil))
	if !IsCode(err, CodeInternal) {
		t.Fatalf("panic 的回执 err = %v, want code=%s", err, CodeInternal)
	}
	if !strings.Contains(err.Error(), "panic") {
		t.Fatalf("回执信息没有指明 panic: %q", err.Error())
	}
	// 计数与日志：panic 既进指标也进日志（栈只进日志，不上线）。
	wsWaitMetric(t, srv, "sump_panics_total", func(m Metric) bool {
		return m.Labels["method"] == "boom" && m.Value >= 1
	})
	if !strings.Contains(logged.String(), "处理方 panic 已隔离") || !strings.Contains(logged.String(), "处理器炸了") {
		t.Fatalf("panic 没有落日志: %q", logged.String())
	}
	// 请求记账照常收敛到 internal，inflight 归零：说明 runCall 走完了而不是半途而死。
	wsWaitMetric(t, srv, "sump_requests_total", func(m Metric) bool {
		return m.Labels["method"] == "boom" && m.Labels["code"] == CodeInternal && m.Value >= 1
	})
	if got := gaugeValue(srv.Metrics().Snapshot(), "sump_inflight_requests", "boom"); got != 0 {
		t.Fatalf("panic 后 inflight = %v, want 0（并发槽必须回收）", got)
	}
	// 同一条连接还能用：panic 不该带走服务端的接收循环。
	if _, err := c.Call(wsCtx(t), wsCall(t, AgentRef("client"), "echo", nil)); err != nil {
		t.Fatalf("panic 之后同一连接调用失败: %v", err)
	}
}

// TestServerPanicAfterReplyDoesNotDoubleSend 钉住「一次 call 只有一帧回执」在 panic 路径上同样成立：
// 已经回过 ok 的处理方再 panic，不得补第二帧，但 panic 本身仍要计数。
func TestServerPanicAfterReplyDoesNotDoubleSend(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("late")))
	srv.Handle("late", func(_ context.Context, c *Call) error {
		if err := c.Reply("done"); err != nil {
			return err
		}
		panic("回复之后才炸")
	})

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	wsWriteFrame(t, conn, wsCall(t, AgentRef("client"), "late", nil))
	resp := wsReadFrame(t, conn)
	if resp.Type != TypeReply || resp.Status != StatusOK {
		t.Fatalf("首帧回执不符: %+v", resp)
	}
	wsExpectNoFrame(t, conn, 300*time.Millisecond)
	wsWaitMetric(t, srv, "sump_panics_total", func(m Metric) bool {
		return m.Labels["method"] == "late" && m.Value == 1
	})
}

// TestServerEventPanicCountedOnce 钉住同一类事件只记一处：event 处理方 panic 时
// 进 sump_panics_total，不再另外进 sump_event_errors_total（那会是两个指标记一件事）。
func TestServerEventPanicCountedOnce(t *testing.T) {
	var logged lockedBuffer
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("evt"), wsTestMethod("echo")), WithLogger(log.New(&logged, "", 0)))
	var called atomic.Bool
	srv.Handle("evt", func(context.Context, *Call) error {
		called.Store(true)
		panic("event 处理器炸了")
	})
	srv.Handle("echo", func(_ context.Context, c *Call) error { return c.Reply("ok") })

	c := wsDialClient(t, url)
	ev, err := NewEvent(wsTestWork, AgentRef("client"), ServiceRef(wsTestService), "evt", nil)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	if err := c.Send(ev); err != nil {
		t.Fatalf("Send: %v", err)
	}
	wsWaitMetric(t, srv, "sump_panics_total", func(m Metric) bool {
		return m.Labels["method"] == "evt" && m.Value >= 1
	})
	if !called.Load() {
		t.Fatal("event 处理方没有被调用")
	}
	for _, m := range srv.Metrics().Snapshot() {
		if m.Name == "sump_event_errors_total" && m.Labels["method"] == "evt" {
			t.Fatalf("panic 被记了两次（sump_panics_total 之外还有 %s）", m.Name)
		}
	}
	if _, err := c.Call(wsCtx(t), wsCall(t, AgentRef("client"), "echo", nil)); err != nil {
		t.Fatalf("event panic 之后连接不可用: %v", err)
	}
	if !strings.Contains(logged.String(), "event 处理器炸了") {
		t.Fatalf("event panic 没有落日志: %q", logged.String())
	}
}

// TestServerUndecodableFrameCountedAndLogged 钉住「解不开的帧不再静默 return」：
// 计数 sump_dropped_total{reason="undecodable"}，并把原因写进日志。
func TestServerUndecodableFrameCountedAndLogged(t *testing.T) {
	var logged lockedBuffer
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")), WithLogger(log.New(&logged, "", 0)))
	var called atomic.Bool
	srv.Handle("echo", func(context.Context, *Call) error {
		called.Store(true)
		return nil
	})

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	junk := []byte("这一串不是 msgpack 帧")
	if _, err := Unmarshal(junk); err == nil {
		t.Fatal("用例前提不成立：这串字节竟然能解成帧")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, junk); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	waitReasonCounter(t, srv, "sump_dropped_total", "undecodable", 1)
	if !strings.Contains(logged.String(), "丢弃入站帧") || !strings.Contains(logged.String(), "undecodable") {
		t.Fatalf("丢弃帧没有落日志: %q", logged.String())
	}
	// 日志只留原因，不留原始字节（不打 body 全文是 spec §12.5 的纪律）。
	if strings.Contains(logged.String(), string(junk)) {
		t.Fatalf("原始字节被写进了日志: %q", logged.String())
	}
	// 调用方不会空等：这条连接被结束，读侧必须看到错误。
	if err := conn.SetReadDeadline(time.Now().Add(wsTestTimeout)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, _, err := conn.ReadMessage(); err == nil {
		t.Fatal("解不开的帧之后连接仍然可用：调用方会一直等下去")
	}
	if called.Load() {
		t.Fatal("解不开的帧竟然进了处理方")
	}
}

// TestServerInvalidCallAndEventShareOneAccounting 钉住「一类事件一套待遇」：
// call 与 event 校验失败是同一种事件，必须落在同一条 sump_dropped_total 序列上，
// 不得为「非 call 帧」另立第二个计数器；call 额外多一帧回执只是为了不让调用方空等。
func TestServerInvalidCallAndEventShareOneAccounting(t *testing.T) {
	var logged lockedBuffer
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")), WithLogger(log.New(&logged, "", 0)))

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	// 两条都缺 method（非法），差别只在帧类型。
	badCall := Frame{V: ProtocolVersion, ID: "bad-call", Type: TypeCall, Work: wsTestWork, From: AgentRef("client"), To: ServiceRef(wsTestService)}
	badEvent := Frame{V: ProtocolVersion, ID: "bad-event", Type: TypeEvent, Work: wsTestWork, From: AgentRef("client"), To: ServiceRef(wsTestService)}
	wsWriteFrame(t, conn, badCall)

	resp := wsReadFrame(t, conn)
	if resp.Type != TypeReply || resp.Status != StatusError {
		t.Fatalf("非法 call 的回执不符: %+v", resp)
	}
	if resp.Error == nil || !IsCode(resp.Error, CodeInvalidArgument) {
		t.Fatalf("非法 call 错误码 = %+v, want invalid_argument", resp.Error)
	}

	wsWriteFrame(t, conn, badEvent)
	waitReasonCounter(t, srv, "sump_dropped_total", "invalid_frame", 2)

	// 一个事件只有一个指标名：带 reason=invalid_frame 的序列必须只属于 sump_dropped_total。
	names := map[string]bool{}
	for _, m := range srv.Metrics().Snapshot() {
		if _, ok := m.Labels["reason"]; ok && m.Labels["reason"] == "invalid_frame" {
			names[m.Name] = true
		}
	}
	if len(names) != 1 || !names["sump_dropped_total"] {
		t.Fatalf("校验失败的记账散到了多个指标: %+v", names)
	}
	logs := logged.String()
	if !strings.Contains(logs, "type=\"call\"") || !strings.Contains(logs, "type=\"event\"") {
		t.Fatalf("丢弃日志没有覆盖两类帧: %q", logs)
	}
}

// TestServerMaxBodyCallAndEventShareOneAccounting 钉住超限帧同样一套待遇：
// 改之前只有 event 计数、call 只回错误不计数——同一种「超限被丢」却是两种账。
func TestServerMaxBodyCallAndEventShareOneAccounting(t *testing.T) {
	card := wsTestCard(wsTestMethod("echo"), wsTestMethod("evt"))
	card.Limits.MaxBody = 256
	srv, url := wsStartServer(t, card)
	var called atomic.Int32
	srv.Handle("echo", func(context.Context, *Call) error {
		called.Add(1)
		return nil
	})
	srv.Handle("evt", func(context.Context, *Call) error {
		called.Add(1)
		return nil
	})

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	wsWriteFrame(t, conn, Frame{
		V: ProtocolVersion, ID: "big-call", Type: TypeCall, Work: wsTestWork,
		From: AgentRef("client"), To: ServiceRef(wsTestService), Method: "echo", Body: []byte(strings.Repeat("x", 257)),
	})
	resp := wsReadFrame(t, conn)
	if resp.Status != StatusError || resp.Error == nil || !IsCode(resp.Error, CodeResourceExhausted) {
		t.Fatalf("超限 call 回执不符: %+v", resp)
	}
	wsWriteFrame(t, conn, Frame{
		V: ProtocolVersion, ID: "big-event", Type: TypeEvent, Work: wsTestWork,
		From: AgentRef("client"), To: ServiceRef(wsTestService), Method: "evt", Body: []byte(strings.Repeat("x", 257)),
	})
	waitReasonCounter(t, srv, "sump_dropped_total", "max_body", 2)
	if got := called.Load(); got != 0 {
		t.Fatalf("超限帧进了处理方 %d 次", got)
	}
}

// TestServerUnhandledFrameTypeCounted 钉住最后一类静默丢弃：结构合法但服务端没有分支的帧
// （reply / stream 打到 /spv2）也必须计数 + 落日志。
func TestServerUnhandledFrameTypeCounted(t *testing.T) {
	var logged lockedBuffer
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")), WithLogger(log.New(&logged, "", 0)))

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	replyFrame := Frame{
		V: ProtocolVersion, ID: "stray-reply", Type: TypeReply, Work: wsTestWork,
		From: ServiceRef(wsTestService), To: AgentRef("client"), ReplyTo: "nobody", Status: StatusOK,
	}
	if err := replyFrame.Validate(); err != nil {
		t.Fatalf("用例前提不成立：这帧本身非法: %v", err)
	}
	wsWriteFrame(t, conn, replyFrame)

	waitReasonCounter(t, srv, "sump_dropped_total", "unhandled_frame", 1)
	if !strings.Contains(logged.String(), "unhandled_frame") {
		t.Fatalf("没有分发分支的帧没有落日志: %q", logged.String())
	}
}

// TestServerLoggerDefaultsWhenNotInjected 保证不接 WithLogger 时也不会因为 logger 为 nil 而二次 panic：
// 默认落点是 log.Default()，记账照常。
func TestServerLoggerDefaultsWhenNotInjected(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	junk := []byte("still-not-a-frame")
	if _, err := Unmarshal(junk); err == nil {
		t.Fatal("用例前提不成立：这串字节竟然能解成帧")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, junk); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	waitReasonCounter(t, srv, "sump_dropped_total", "undecodable", 1)
}
