package spv2

// WebSocket client/server 的端到端测试。
// 只使用本机 httptest 服务，不访问外网；等待一律用 channel / context 超时，不用长 sleep。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const (
	wsTestService = "tester"
	wsTestWork    = "w1"
	wsTestTimeout = 5 * time.Second
)

type wsEchoRequest struct {
	Text string `msgpack:"text" json:"text"`
}

type wsEchoResponse struct {
	Text   string `msgpack:"text" json:"text"`
	Method string `msgpack:"method" json:"method"`
}

type wsProgressEvent struct {
	Percent int `msgpack:"percent" json:"percent"`
}

type wsStreamChunk struct {
	Text string `msgpack:"text" json:"text"`
}

// wsTestCard 构造一张最小合法服务卡。
func wsTestCard(methods ...Method) Card {
	return Card{
		Protocol:     ProtocolVersion,
		Name:         wsTestService,
		Actor:        ServiceRef(wsTestService),
		Version:      "0.1.0",
		Capabilities: []string{"msgpack", "json", "stream"},
		Methods:      methods,
		Limits:       Limits{MaxBody: 1 << 20, MaxStream: 32},
		Observability: Observability{
			Health: true, Metrics: true, Format: "spv2.metrics.v1",
		},
	}
}

func wsTestMethod(name string) Method {
	return Method{Name: name, Shape: ShapeCall, Version: 1}
}

// wsStartServer 起 httptest 服务并把 Handler 挂到 /spv2，返回服务端与客户端 URL。
func wsStartServer(t *testing.T, card Card, opts ...ServerOption) (*Server, string) {
	t.Helper()
	srv, err := NewServer(card, opts...)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	mux := http.NewServeMux()
	mux.Handle("/spv2", srv.Handler())
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return srv, "ws" + strings.TrimPrefix(ts.URL, "http") + "/spv2"
}

func wsDialClient(t *testing.T, url string, opts ...ClientOption) *Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wsTestTimeout)
	defer cancel()
	c, err := Dial(ctx, url, opts...)
	if err != nil {
		t.Fatalf("Dial(%s): %v", url, err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func wsDialRaw(t *testing.T, url string) *websocket.Conn {
	t.Helper()
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("raw dial(%s): %v", url, err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// wsReadFrame 从裸连接读一帧，带读超时，避免测试挂死。
func wsReadFrame(t *testing.T, conn *websocket.Conn) Frame {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(wsTestTimeout))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read frame: %v", err)
	}
	f, err := Unmarshal(data)
	if err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	return f
}

func wsCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wsTestTimeout)
	t.Cleanup(cancel)
	return ctx
}

// wsWaitMetric 等待指标出现：服务端在 handler 返回后才记账，reply 可能先于指标可见。
// 用 1ms 轮询 + 2s 上限，既确定性又不会长 sleep。
func wsWaitMetric(t *testing.T, srv *Server, name string, match func(Metric) bool) Metric {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, m := range srv.Metrics().Snapshot() {
			if m.Name == name && match(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待指标 %s 超时: %+v", name, srv.Metrics().Snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

func wsCall(t *testing.T, from Ref, method string, body any) Frame {
	t.Helper()
	f, err := NewCall(wsTestWork, from, ServiceRef(wsTestService), method, body)
	if err != nil {
		t.Fatalf("NewCall(%s): %v", method, err)
	}
	return f
}

func TestTransportServerRejectsInvalidCard(t *testing.T) {
	base := func() Card { return wsTestCard(wsTestMethod("echo")) }

	cases := []struct {
		name   string
		mutate func(*Card)
	}{
		{"协议不匹配", func(c *Card) { c.Protocol = "spv1" }},
		{"缺少服务名", func(c *Card) { c.Name = "" }},
		{"actor 不是 service", func(c *Card) { c.Actor = AgentRef("tester") }},
		{"method 重复", func(c *Card) { c.Methods = append(c.Methods, c.Methods[0]) }},
		{"method shape 非法", func(c *Card) { c.Methods[0].Shape = "push" }},
		{"tool 缺 input schema", func(c *Card) {
			c.Methods[0].Tool = true
			c.Methods[0].Input = nil
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := base()
			tc.mutate(&card)
			if _, err := NewServer(card); err == nil {
				t.Fatal("NewServer 接受了非法卡片")
			}
		})
	}

	if _, err := NewServer(base()); err != nil {
		t.Fatalf("NewServer 拒绝了合法卡片: %v", err)
	}
}

func TestTransportHelloReturnsCard(t *testing.T) {
	card := wsTestCard(wsTestMethod("echo"))
	_, url := wsStartServer(t, card)
	c := wsDialClient(t, url)
	ctx := wsCtx(t)

	got, err := c.Hello(ctx, AgentRef("client"), ServiceRef(wsTestService))
	if err != nil {
		t.Fatalf("Hello: %v", err)
	}
	if got.Protocol != ProtocolVersion {
		t.Fatalf("card.protocol = %q, want %q", got.Protocol, ProtocolVersion)
	}
	if got.Name != card.Name || got.Actor != card.Actor {
		t.Fatalf("card 身份不符: %+v", got)
	}
	if _, ok := got.Method("echo"); !ok {
		t.Fatalf("hello 返回的卡片缺 echo method: %+v", got.Methods)
	}
}

func TestTransportCallEcho(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	srv.Handle("echo", func(_ context.Context, call *Call) error {
		var req wsEchoRequest
		if err := call.Frame.DecodeBody(&req); err != nil {
			return ErrCode(CodeInvalidArgument, "echo body 解码失败: "+err.Error(), false, nil)
		}
		if call.WorkID() != wsTestWork {
			return ErrCode(CodeInvalidArgument, "work 不符: "+call.WorkID(), false, nil)
		}
		return call.Reply(wsEchoResponse{Text: req.Text, Method: call.Frame.Method})
	})

	c := wsDialClient(t, url)
	ctx := wsCtx(t)
	call := wsCall(t, AgentRef("client"), "echo", wsEchoRequest{Text: "ping"})

	resp, err := c.Call(ctx, call)
	if err != nil {
		t.Fatalf("Call(echo): %v", err)
	}
	if resp.Type != TypeReply || resp.Status != StatusOK {
		t.Fatalf("reply 帧头不符: %+v", resp)
	}
	if resp.ReplyTo != call.ID || resp.Work != wsTestWork {
		t.Fatalf("reply 关联字段不符: reply_to=%q work=%q", resp.ReplyTo, resp.Work)
	}
	if resp.From != ServiceRef(wsTestService) || resp.To != AgentRef("client") {
		t.Fatalf("reply from/to 不符: from=%+v to=%+v", resp.From, resp.To)
	}

	var out wsEchoResponse
	if err := resp.DecodeBody(&out); err != nil {
		t.Fatalf("DecodeBody: %v", err)
	}
	if out.Text != "ping" || out.Method != "echo" {
		t.Fatalf("echo 结果 = %+v", out)
	}
}

func TestTransportUnknownMethod(t *testing.T) {
	_, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	c := wsDialClient(t, url)
	ctx := wsCtx(t)

	call := wsCall(t, AgentRef("client"), "no_such_method", nil)
	resp, err := c.Call(ctx, call)
	if err == nil {
		t.Fatal("未知 method 应当返回错误")
	}
	if !IsCode(err, CodeNotFound) {
		t.Fatalf("want not_found, got %v", err)
	}
	if resp.Status != StatusError || resp.Error == nil || resp.Error.Code != CodeNotFound {
		t.Fatalf("错误 reply 不符: %+v", resp)
	}
	if resp.ReplyTo != call.ID {
		t.Fatalf("reply_to = %q, want %q", resp.ReplyTo, call.ID)
	}
}

func TestTransportWrongTarget(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	var handled atomic.Int32
	srv.Handle("echo", func(_ context.Context, call *Call) error {
		handled.Add(1)
		return call.Reply(wsEchoResponse{Text: "x", Method: call.Frame.Method})
	})

	c := wsDialClient(t, url)
	ctx := wsCtx(t)

	call, err := NewCall(wsTestWork, AgentRef("client"), ServiceRef("other-service"), "echo", nil)
	if err != nil {
		t.Fatalf("NewCall: %v", err)
	}
	resp, err := c.Call(ctx, call)
	if err == nil || !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("目标不是本服务应回 invalid_argument, got %v", err)
	}
	if resp.Error == nil || resp.Error.Code != CodeInvalidArgument {
		t.Fatalf("错误 reply 不符: %+v", resp)
	}
	if n := handled.Load(); n != 0 {
		t.Fatalf("handler 不应被调用，实际 %d 次", n)
	}
}

func TestTransportAgentOwnerAuthority(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	var handled atomic.Int32
	srv.Handle("echo", func(_ context.Context, call *Call) error {
		handled.Add(1)
		return call.Reply(wsEchoResponse{Text: "x", Method: call.Frame.Method})
	})

	// 客户端本地校验就应拒绝 agent 冒充 owner。
	if _, err := NewCall(wsTestWork, AgentRef("agent-x").WithAuthority(AuthorityOwner), ServiceRef(wsTestService), "echo", nil); err == nil || !IsCode(err, CodePermissionDenied) {
		t.Fatalf("NewCall 应拒绝 agent authority=owner, got %v", err)
	}

	// 绕过客户端，直接把同样的一帧发给服务端：服务端也必须拒绝且不调用 handler。
	frame := Frame{
		V: ProtocolVersion, ID: NewID(), Type: TypeCall, Work: wsTestWork,
		From:        AgentRef("agent-x").WithAuthority(AuthorityOwner),
		To:          ServiceRef(wsTestService),
		Method:      "echo",
		ContentType: ContentTypeMsgPack,
	}
	if err := frame.Validate(); err == nil {
		t.Fatal("校验器应拒绝 agent authority=owner")
	}
	data, err := Marshal(frame)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	raw := wsDialRaw(t, url)
	_ = raw.SetWriteDeadline(time.Now().Add(wsTestTimeout))
	if err := raw.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
	resp := wsReadFrame(t, raw)
	if resp.Type != TypeReply || resp.Status != StatusError || resp.Error == nil {
		t.Fatalf("错误 reply 不符: %+v", resp)
	}
	if resp.Error.Code != CodePermissionDenied && resp.Error.Code != CodeInvalidArgument {
		t.Fatalf("want permission_denied/invalid_argument, got %q", resp.Error.Code)
	}
	if resp.ReplyTo != frame.ID {
		t.Fatalf("reply_to = %q, want %q", resp.ReplyTo, frame.ID)
	}
	if n := handled.Load(); n != 0 {
		t.Fatalf("handler 不应被调用，实际 %d 次", n)
	}
}

func TestTransportReplyError(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("fail")))
	srv.Handle("fail", func(_ context.Context, call *Call) error {
		return call.ReplyError(CodeInvalidArgument, "bad input", false, map[string]any{"field": "text"})
	})

	c := wsDialClient(t, url)
	ctx := wsCtx(t)
	call := wsCall(t, AgentRef("client"), "fail", nil)

	resp, err := c.Call(ctx, call)
	if err == nil || !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("handler ReplyError 应回 invalid_argument, got %v", err)
	}
	if resp.Status != StatusError || resp.Error == nil {
		t.Fatalf("错误 reply 不符: %+v", resp)
	}
	if resp.ReplyTo != call.ID || resp.Error.Message != "bad input" || resp.Error.Retryable {
		t.Fatalf("错误字段不符: %+v", resp.Error)
	}
	if got := resp.Error.Details["field"]; got != "text" {
		t.Fatalf("details 不符: %+v", resp.Error.Details)
	}
}

func TestTransportEvent(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("emit")))
	srv.Handle("emit", func(_ context.Context, call *Call) error {
		if err := call.SendEvent("progress", wsProgressEvent{Percent: 42}); err != nil {
			return err
		}
		return call.Reply(wsEchoResponse{Text: "done"})
	})

	events := make(chan Frame, 4)
	c := wsDialClient(t, url, WithOnEvent(func(f Frame) { events <- f }))
	ctx := wsCtx(t)
	call := wsCall(t, AgentRef("client"), "emit", nil)

	resp, err := c.Call(ctx, call)
	if err != nil {
		t.Fatalf("Call(emit): %v", err)
	}
	if resp.Status != StatusOK {
		t.Fatalf("reply status = %q", resp.Status)
	}

	select {
	case ev := <-events:
		if ev.Type != TypeEvent || ev.Method != "progress" {
			t.Fatalf("event 帧头不符: %+v", ev)
		}
		if ev.Work != wsTestWork {
			t.Fatalf("event.work = %q, want %q", ev.Work, wsTestWork)
		}
		if ev.From != ServiceRef(wsTestService) || ev.To != AgentRef("client") {
			t.Fatalf("event from/to 不符: from=%+v to=%+v", ev.From, ev.To)
		}
		var p wsProgressEvent
		if err := ev.DecodeBody(&p); err != nil {
			t.Fatalf("event DecodeBody: %v", err)
		}
		if p.Percent != 42 {
			t.Fatalf("event body = %+v", p)
		}
	case <-ctx.Done():
		t.Fatal("未收到 event")
	}
}

func TestTransportStream(t *testing.T) {
	m := wsTestMethod("count")
	m.Streaming = true
	srv, url := wsStartServer(t, wsTestCard(m))
	srv.Handle("count", func(_ context.Context, call *Call) error {
		parts := []string{"a", "b", "c"}
		for i, p := range parts {
			end := i == len(parts)-1
			status := ""
			if end {
				status = StatusOK
			}
			if err := call.SendStream("s1", int64(i+1), end, wsStreamChunk{Text: p}, status); err != nil {
				return err
			}
		}
		return nil
	})

	c := wsDialClient(t, url)
	ctx := wsCtx(t)
	call := wsCall(t, AgentRef("client"), "count", nil)
	ch, err := c.Stream(ctx, call)
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}

	var frames []Frame
	for f := range ch {
		frames = append(frames, f)
	}
	if len(frames) != 3 {
		t.Fatalf("收到 %d 块, want 3", len(frames))
	}
	want := []string{"a", "b", "c"}
	for i, f := range frames {
		if f.Type != TypeStream {
			t.Fatalf("第 %d 帧 type = %q", i, f.Type)
		}
		if f.ReplyTo != call.ID {
			t.Fatalf("第 %d 帧 reply_to = %q, want %q", i, f.ReplyTo, call.ID)
		}
		if f.Stream == nil {
			t.Fatalf("第 %d 帧缺 stream", i)
		}
		if f.Stream.ID != "s1" || f.Stream.Seq != int64(i+1) {
			t.Fatalf("第 %d 帧 stream = %+v", i, *f.Stream)
		}
		wantEnd := i == len(frames)-1
		if f.Stream.End != wantEnd {
			t.Fatalf("第 %d 帧 end = %v, want %v", i, f.Stream.End, wantEnd)
		}
		if wantEnd {
			if f.Status != StatusOK {
				t.Fatalf("末块 status = %q, want %q", f.Status, StatusOK)
			}
		} else if f.Status != "" {
			t.Fatalf("中间块不应带 status, got %q", f.Status)
		}
		var chunk wsStreamChunk
		if err := f.DecodeBody(&chunk); err != nil {
			t.Fatalf("第 %d 块 DecodeBody: %v", i, err)
		}
		if chunk.Text != want[i] {
			t.Fatalf("第 %d 块 body = %+v", i, chunk)
		}
	}
}

func TestTransportMetrics(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	srv.Handle("echo", func(_ context.Context, call *Call) error {
		return call.Reply(wsEchoResponse{Text: "x", Method: call.Frame.Method})
	})

	c := wsDialClient(t, url)
	ctx := wsCtx(t)
	if _, err := c.Call(ctx, wsCall(t, AgentRef("client"), "echo", wsEchoRequest{Text: "a"})); err != nil {
		t.Fatalf("echo: %v", err)
	}

	got := wsWaitMetric(t, srv, "sump_requests_total", func(m Metric) bool {
		return m.Type == "counter" &&
			m.Labels["method"] == "echo" && m.Labels["code"] == StatusOK && m.Value >= 1
	})
	if got.Value < 1 {
		t.Fatalf("sump_requests_total = %+v", got)
	}

	// metrics 是 well-known method，卡片里不必声明。
	var mr MetricsResponse
	if err := c.CallDecode(ctx, wsCall(t, AgentRef("client"), "metrics", nil), &mr); err != nil {
		t.Fatalf("CallDecode(metrics): %v", err)
	}
	if mr.Service != wsTestService {
		t.Fatalf("metrics.service = %q", mr.Service)
	}
	hasRequests := false
	for _, m := range mr.Metrics {
		if m.Name == "sump_requests_total" {
			hasRequests = true
		}
	}
	if !hasRequests {
		t.Fatalf("metrics reply 缺 sump_requests_total: %+v", mr.Metrics)
	}
}

func TestTransportMaxInflight(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("slow")),
		WithMaxInflight(1), WithActionTimeout(wsTestTimeout))

	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	srv.Handle("slow", func(ctx context.Context, call *Call) error {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-release:
		case <-ctx.Done():
			return ctx.Err()
		}
		return call.Reply(wsEchoResponse{Text: "slow"})
	})

	c := wsDialClient(t, url)
	ctx := wsCtx(t)
	firstCall := wsCall(t, AgentRef("client"), "slow", nil)
	secondCall := wsCall(t, AgentRef("client"), "slow", nil)

	firstResult := make(chan error, 1)
	go func() {
		_, err := c.Call(ctx, firstCall)
		firstResult <- err
	}()

	// 第一个 call 进入 handler 说明服务端 semaphore 已被占用。
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("第一个 call 未进入 handler")
	}

	// 此时第二个 call 必须被 inflight 限制立即拒绝，而不是排队。
	_, err := c.Call(ctx, secondCall)
	if err == nil || !IsCode(err, CodeResourceExhausted) {
		t.Fatalf("第二个 call 应被 resource_exhausted 拒绝, got %v", err)
	}

	close(release)
	if err := <-firstResult; err != nil {
		t.Fatalf("第一个 call 失败: %v", err)
	}
}
