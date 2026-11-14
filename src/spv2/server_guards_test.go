package spv2

// server 侧必须真的执行帧校验与「一次 call 只回一帧」的守卫。
// 这里的用例都是裸 WS 连接：绕过 client 的本地校验与关联簿记，
// 让服务端自己那一半行为可观测（runCall 会因为 Replied() 而吞掉第二帧，
// 所以双回复只能像下面这样直接调 Reply / ReplyError 才测得到）。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestServerRejectsWrongVersionOnWire 钉住「只有一种线协议：SPv2.1 帧（v: "spv2.1"）」。
// 版本写错的 call 必须被服务端拒绝、且不得进入 handler；
// 错误 reply 的 reply_to 必须指回原帧（结果归属不可丢失）。
func TestServerRejectsWrongVersionOnWire(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("echo")))
	var handled atomic.Int32
	srv.Handle("echo", func(_ context.Context, c *Call) error {
		handled.Add(1)
		return c.Reply("ok")
	})

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	f := Frame{
		V: "spv1", ID: "bad-v-1", Type: TypeCall, Work: wsTestWork,
		From: AgentRef("client"), To: ServiceRef(wsTestService),
		Method: "echo", ContentType: ContentTypeMsgPack,
	}
	data, err := Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	resp := wsReadFrame(t, conn)
	if resp.Type != TypeReply || resp.Status != StatusError {
		t.Fatalf("错误 reply 不符: %+v", resp)
	}
	if resp.Error == nil || !IsCode(resp.Error, CodeInvalidArgument) {
		t.Fatalf("错误码 = %+v, want invalid_argument", resp.Error)
	}
	if !strings.Contains(resp.Error.Message, "v 必须是") {
		t.Fatalf("错误信息没有指出版本问题: %q", resp.Error.Message)
	}
	if resp.ReplyTo != f.ID {
		t.Fatalf("reply_to = %q, want %q", resp.ReplyTo, f.ID)
	}
	if n := handled.Load(); n != 0 {
		t.Fatalf("非法版本的 call 进了 handler %d 次", n)
	}
}

// TestServerCallCannotReplyTwice 钉住「一次 call 只有一帧归属明确的 reply」。
// 第一次 Reply 成功后，再 Reply / 再 ReplyError 都必须返回 CodeConflict，
// 并且第二帧绝不能上线——否则调用方会收到两帧，reply_to 关联直接崩掉。
//
// 变异提示：把 server.go 里 Reply 与 ReplyError 的 replied 判断各自删掉，
// 本用例的两个分支会分别变红（一个是第二次 Reply，一个是 Reply 之后 ReplyError）。
func TestServerCallCannotReplyTwice(t *testing.T) {
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("twice")))
	errs := make(chan error, 2)
	srv.Handle("twice", func(_ context.Context, c *Call) error {
		if err := c.Reply("first"); err != nil {
			errs <- err
			errs <- err
			return nil
		}
		errs <- c.Reply("second")
		errs <- c.ReplyError(CodeInternal, "boom", false, nil)
		return nil
	})

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	f := Frame{
		V: ProtocolVersion, ID: "twice-1", Type: TypeCall, Work: wsTestWork,
		From: AgentRef("client"), To: ServiceRef(wsTestService),
		Method: "twice", ContentType: ContentTypeMsgPack,
	}
	data, err := Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	resp := wsReadFrame(t, conn)
	if resp.Type != TypeReply || resp.Status != StatusOK {
		t.Fatalf("首帧 reply 不符: %+v", resp)
	}
	var out string
	if err := resp.DecodeBody(&out); err != nil {
		t.Fatalf("DecodeBody: %v", err)
	}
	if out != "first" {
		t.Fatalf("首帧 body = %q, want %q", out, "first")
	}

	// 等 handler 把两次越界回应的错误都交出来，再检查线上有没有多余帧。
	for i, want := range []string{"Reply", "ReplyError"} {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatalf("%s 第二次回应竟然成功了（guard 失效）", want)
			}
			if !IsCode(err, CodeConflict) {
				t.Fatalf("%s 越界回应 %d: 错误码 = %v, want conflict", want, i, err)
			}
			if !strings.Contains(err.Error(), "call already replied") {
				t.Fatalf("%s 越界回应信息 = %q, 应指明 call already replied", want, err)
			}
		case <-time.After(wsTestTimeout):
			t.Fatalf("%s 没有返回越界回应的错误", want)
		}
	}

	// 线上只应有一帧 reply：此刻若还能读到帧，说明重复回应被发了出去。
	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, extra, err := conn.ReadMessage(); err == nil {
		t.Fatalf("重复回应上线了: %q", extra)
	}
}
