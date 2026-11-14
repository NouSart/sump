package spv2

// spec §10：流可以被 call method=cancel（带 stream）取消——服务端释放账本，
// 客户端本地通道关闭；重复取消返回 0，而不是假装又取消了一次。

import (
	"context"
	"testing"
	"time"
)

func TestStreamCancelReleasesLedger(t *testing.T) {
	card := wsTestCard(wsTestMethod("slow"))
	srv, url := wsStartServer(t, card)
	release := make(chan struct{})
	srv.Handle("slow", func(_ context.Context, c *Call) error {
		if err := c.SendStream("s1", 1, false, "a", ""); err != nil {
			return err
		}
		<-release
		// 取消后账本已释放：这一帧会因 seq 不连续被拒，handler 直接返回。
		_ = c.SendStream("s1", 2, true, "b", StatusOK)
		return nil
	})
	c := wsDialClient(t, url)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := NewCall(wsTestWork, AgentRef("client"), ServiceRef(wsTestService), "slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.Stream(ctx, call)
	if err != nil {
		t.Fatal(err)
	}
	var first Frame
	select {
	case first = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("没有收到第一帧流")
	}
	if first.Stream == nil {
		t.Fatalf("第一帧不是流帧: %+v", first)
	}
	n, err := c.CancelStream(ctx, AgentRef("client"), ServiceRef(wsTestService), wsTestWork, first.Stream.ID)
	if err != nil {
		t.Fatalf("CancelStream 失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("第一次取消应释放 1 条流，got %d", n)
	}
	// 本地通道必须关闭（调用方 range 能退出）。
	select {
	case _, ok := <-ch:
		if ok {
			t.Fatal("取消后不应再收到流帧")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("取消后本地流通道没有关闭")
	}
	// 再取消一次：账本已空，必须回 0（不虚报取消成功）。
	n2, err := c.CancelStream(ctx, AgentRef("client"), ServiceRef(wsTestService), wsTestWork, first.Stream.ID)
	if err != nil {
		t.Fatalf("第二次 CancelStream 失败: %v", err)
	}
	if !hasMetric(srv.Metrics().Snapshot(), "sump_stream_cancelled_total", map[string]string{"outcome": "transport"}) {
		t.Fatalf("transport cancel 没有进指标: %+v", srv.Metrics().Snapshot())
	}
	if n2 != 0 {
		t.Fatalf("重复取消应释放 0 条，got %d", n2)
	}
	close(release)
}
