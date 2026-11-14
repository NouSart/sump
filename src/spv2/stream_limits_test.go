package spv2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// spec §3.5/§10：流式必须有界——并发流上限、单流字节上限、seq 从 1 递增。

func TestServerStreamConcurrentLimit(t *testing.T) {
	card := wsTestCard(wsTestMethod("stream"))
	card.Limits.MaxStream = 1
	srv, url := wsStartServer(t, card)
	errCh := make(chan error, 2)
	srv.Handle("stream", func(_ context.Context, c *Call) error {
		if err := c.SendStream("s1", 1, false, "a", ""); err != nil {
			return err
		}
		errCh <- c.SendStream("s2", 1, false, "b", "") // 第二个流必须被拒
		if err := c.SendStream("s1", 2, true, "a", StatusOK); err != nil {
			return err
		}
		// s1 结束后槽位释放：s2 从头再来应当成功。
		if err := c.SendStream("s2", 1, false, "b", ""); err != nil {
			return err
		}
		return c.SendStream("s2", 2, true, "b", StatusOK)
	})
	c := wsDialClient(t, url)
	ch, err := c.Stream(wsCtx(t), wsCall(t, AgentRef("client"), "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	select {
	case err := <-errCh:
		if !IsCode(err, CodeResourceExhausted) {
			t.Fatalf("第二个流 err = %v, want resource_exhausted", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有拿到第二个流的拒绝错误")
	}
}

func TestServerStreamBufferLimit(t *testing.T) {
	card := wsTestCard(wsTestMethod("stream"))
	srv, url := wsStartServer(t, card, WithMaxStreamBuffer(4))
	errCh := make(chan error, 1)
	srv.Handle("stream", func(_ context.Context, c *Call) error {
		errCh <- c.SendStream("s", 1, false, "0123456789", "")
		return c.SendStream("s", 1, true, "x", StatusOK)
	})
	c := wsDialClient(t, url)
	ch, err := c.Stream(wsCtx(t), wsCall(t, AgentRef("client"), "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	select {
	case err := <-errCh:
		if !IsCode(err, CodeResourceExhausted) {
			t.Fatalf("超 buffer err = %v, want resource_exhausted", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有拿到超 buffer 的拒绝错误")
	}
}

func TestServerStreamSeqMustIncrement(t *testing.T) {
	card := wsTestCard(wsTestMethod("stream"))
	srv, url := wsStartServer(t, card)
	errCh := make(chan error, 1)
	srv.Handle("stream", func(_ context.Context, c *Call) error {
		if err := c.SendStream("s", 1, false, "a", ""); err != nil {
			return err
		}
		errCh <- c.SendStream("s", 3, false, "b", "") // 跳号必须被拒
		return c.SendStream("s", 2, true, "c", StatusOK)
	})
	c := wsDialClient(t, url)
	ch, err := c.Stream(wsCtx(t), wsCall(t, AgentRef("client"), "stream", nil))
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	select {
	case err := <-errCh:
		if !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("跳号 err = %v, want invalid_argument", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("没有拿到跳号的拒绝错误")
	}
}

// 客户端必须能从线上帧发现 seq 缺口/重复并记账（spec §10 的接收侧义务）。
func TestClientStreamGapCounted(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		// 先完成 §5.1 握手：客户端在第一次 Stream 前会自动发 hello。
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		hello, err := Unmarshal(data)
		if err != nil {
			return
		}
		card := testCard()
		card.Name = "fake"
		card.Actor = ServiceRef("fake")
		reply, err := NewReply(hello, ServiceRef("fake"), HelloResponse{Card: card})
		if err != nil {
			return
		}
		if raw, err := Marshal(reply); err == nil {
			_ = ws.WriteMessage(websocket.BinaryMessage, raw)
		}
		_, data, err = ws.ReadMessage()
		if err != nil {
			return
		}
		call, err := Unmarshal(data)
		if err != nil {
			return
		}
		from := ServiceRef("fake")
		send := func(seq int64, end bool, status string) {
			fr, err := NewStreamFrame(call, from, "s1", seq, end, "x", status)
			if err != nil {
				return
			}
			data, err := Marshal(fr)
			if err != nil {
				return
			}
			_ = ws.WriteMessage(websocket.BinaryMessage, data)
		}
		send(1, false, "")
		send(3, false, "") // 缺 seq=2
		send(3, false, "") // 重复 seq=3
		send(4, true, StatusOK)
	}))
	defer ts.Close()
	reg := NewRegistry()
	c, err := Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"), WithClientMetrics(reg))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := NewCall("w1", AgentRef("client"), ServiceRef("fake"), "stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.Stream(context.Background(), f)
	if err != nil {
		t.Fatal(err)
	}
	for range ch {
	}
	missing, duplicate := 0.0, 0.0
	for _, m := range reg.Snapshot() {
		if m.Name != "sump_stream_gap_total" {
			continue
		}
		switch m.Labels["outcome"] {
		case "missing":
			missing += m.Value
		case "duplicate":
			duplicate += m.Value
		}
	}
	if missing < 1 || duplicate < 1 {
		t.Fatalf("gap 记账 missing=%v duplicate=%v, want both >=1", missing, duplicate)
	}
}
