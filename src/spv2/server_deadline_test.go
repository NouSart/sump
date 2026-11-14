package spv2

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// spec §9.3：到点必须回 deadline_exceeded，不得挂住。
func TestServerActionDeadlineRepliesDeadlineExceeded(t *testing.T) {
	release := make(chan struct{})
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("block")), WithActionTimeout(80*time.Millisecond))
	srv.Handle("block", func(ctx context.Context, c *Call) error {
		select {
		case <-release:
		case <-ctx.Done():
		}
		return c.Reply("late")
	})
	t.Cleanup(func() { close(release) })
	c := wsDialClient(t, url)
	f := wsCall(t, AgentRef("client"), "block", nil)
	start := time.Now()
	_, err := c.Call(timeoutCtx(t), f)
	elapsed := time.Since(start)
	if !IsCode(err, CodeDeadlineExceeded) {
		t.Fatalf("err = %v, want deadline_exceeded", err)
	}
	var e *Error
	if !errors.As(err, &e) || !e.Retryable {
		t.Fatalf("deadline_exceeded 必须 retryable=true（spec §8）: %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("回执用了 %v，deadline 没有生效", elapsed)
	}
}

// 处理方完全不看 ctx 时也必须被 deadline 兜住（不能等 handler 自觉）。
func TestServerActionDeadlineCutsStuckHandler(t *testing.T) {
	release := make(chan struct{})
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("stuck")), WithActionTimeout(60*time.Millisecond))
	srv.Handle("stuck", func(context.Context, *Call) error {
		<-release
		return nil
	})
	t.Cleanup(func() { close(release) })
	c := wsDialClient(t, url)
	f := wsCall(t, AgentRef("client"), "stuck", nil)
	_, err := c.Call(timeoutCtx(t), f)
	if !IsCode(err, CodeDeadlineExceeded) {
		t.Fatalf("err = %v, want deadline_exceeded", err)
	}
}

// deadline 之后才到的结果（late result）不得再发终态帧：handler 已过点，回执已经发出。
func TestServerLateStreamAfterDeadlineIsDropped(t *testing.T) {
	release := make(chan struct{})
	var once sync.Once
	releaseFn := func() { once.Do(func() { close(release) }) }
	srv, url := wsStartServer(t, wsTestCard(wsTestMethod("late")), WithActionTimeout(60*time.Millisecond))
	srv.Handle("late", func(_ context.Context, c *Call) error {
		<-release
		// 先发非终态块：回错误终态之后，任何流式块（含中间块）都不该再上线。
		if err := c.SendStream("s1", 1, false, "late", ""); err != nil {
			t.Logf("late 中间块被拒（预期）: %v", err)
		}
		return c.SendStream("s1", 2, true, "late", StatusOK)
	})
	t.Cleanup(releaseFn)

	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)
	wsWriteFrame(t, conn, wsCall(t, AgentRef("client"), "late", nil))
	first := wsReadFrame(t, conn)
	if first.Status != StatusError || !IsCode(first.Error, CodeDeadlineExceeded) {
		t.Fatalf("第一帧 = %+v, want deadline_exceeded", first)
	}
	releaseFn()
	// handler 醒来后试图发 late 帧；服务端必须丢弃（call 已回错误终态）。
	if err := conn.SetReadDeadline(time.Now().Add(300 * time.Millisecond)); err != nil {
		t.Fatalf("SetReadDeadline: %v", err)
	}
	if _, extra, err := conn.ReadMessage(); err == nil {
		t.Fatalf("deadline 之后又收到一帧 late result: %s", extra)
	}
}
