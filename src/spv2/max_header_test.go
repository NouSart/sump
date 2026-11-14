package spv2

// spec §3.5：max_header（默认 16 KiB）超限必须拒绝，不能把大 header 当正常帧收下。

import (
	"context"
	"strings"
	"testing"
)

func TestServerMaxHeaderDefaultsTo16KiB(t *testing.T) {
	srv, err := NewServer(testCard())
	if err != nil {
		t.Fatal(err)
	}
	if got := srv.Card().Limits.MaxHeader; got != 16<<10 {
		t.Fatalf("默认 max_header = %d, want 16384", got)
	}
}

func TestServerRejectsOversizedHeader(t *testing.T) {
	card := wsTestCard(wsTestMethod("echo"))
	card.Limits.MaxHeader = 512
	srv, url := wsStartServer(t, card)
	var called int32
	srv.Handle("echo", func(_ context.Context, c *Call) error {
		called++
		return c.Reply("ok")
	})
	conn := wsDialRaw(t, url)
	wsHandshake(t, conn)

	// method 长到让 header 超过 512 字节：必须回 resource_exhausted，且不进 handler。
	call, err := NewCall("w1", AgentRef("client"), ServiceRef(wsTestService), strings.Repeat("m", 600), nil)
	if err != nil {
		t.Fatal(err)
	}
	wsWriteFrame(t, conn, call)
	resp := wsReadFrame(t, conn)
	if resp.Status != StatusError || !IsCode(resp.Error, CodeResourceExhausted) {
		t.Fatalf("超大 header 回执 = %+v, want resource_exhausted", resp)
	}
	if called != 0 {
		t.Fatal("超大 header 进了 handler")
	}
	waitReasonCounter(t, srv, "sump_dropped_total", "max_header", 1)

	// 正常头仍可用（拒绝不能误伤）。
	ok, err := NewCall("w2", AgentRef("client"), ServiceRef(wsTestService), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	wsWriteFrame(t, conn, ok)
	resp = wsReadFrame(t, conn)
	if resp.Status != StatusOK {
		t.Fatalf("正常头 call 回执 = %+v, want ok", resp)
	}
	if called != 1 {
		t.Fatalf("正常 call 应进 handler 一次，got %d", called)
	}
}
