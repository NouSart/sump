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

// spec §5.1：连接后第一帧必须是 hello；未握手前的业务帧必须被拒，且不能静默丢弃。
func TestServerRequiresHelloFirst(t *testing.T) {
	card := wsTestCard(wsTestMethod("echo"))
	srv, url := wsStartServer(t, card)
	var called int32
	srv.Handle("echo", func(_ context.Context, c *Call) error {
		called++
		return c.Reply("ok")
	})
	conn := wsDialRaw(t, url)

	call, err := NewCall(wsTestWork, AgentRef("client"), ServiceRef(wsTestService), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	wsWriteFrame(t, conn, call)
	resp := wsReadFrame(t, conn)
	if resp.Status != StatusError || !IsCode(resp.Error, CodeInvalidArgument) {
		t.Fatalf("未握手的 call 回执 = %+v, want invalid_argument", resp)
	}
	waitReasonCounter(t, srv, "sump_dropped_total", "no_hello", 1)
	if called != 0 {
		t.Fatal("未握手的 call 进了 handler")
	}

	// 同一连接补上握手后，业务帧恢复可用。
	wsHandshake(t, conn)
	wsWriteFrame(t, conn, call)
	resp = wsReadFrame(t, conn)
	if resp.Status != StatusOK {
		t.Fatalf("握手后的 call 回执 = %+v, want ok", resp)
	}
	if called != 1 {
		t.Fatalf("handler 调用次数 = %d, want 1", called)
	}
}

// 客户端的第一帧必须自动是 hello（调用方不需要手动握手）。
func TestClientAutoHandshakesBeforeFirstCall(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			f, err := Unmarshal(data)
			if err != nil {
				return
			}
			seen = append(seen, f.Method)
			if f.Method == "hello" {
				card := testCard()
				card.Name = "peer"
				card.Actor = ServiceRef("peer")
				reply, err := NewReply(f, ServiceRef("peer"), HelloResponse{Card: card})
				if err != nil {
					return
				}
				raw, _ := Marshal(reply)
				_ = ws.WriteMessage(websocket.BinaryMessage, raw)
				continue
			}
			reply, err := NewReply(f, ServiceRef("peer"), "pong")
			if err != nil {
				return
			}
			raw, _ := Marshal(reply)
			_ = ws.WriteMessage(websocket.BinaryMessage, raw)
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := NewCall("w1", AgentRef("client"), ServiceRef("peer"), "echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, f); err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(seen) < 2 || seen[0] != "hello" || seen[1] != "echo" {
		t.Fatalf("线上前两帧 = %v, want [hello echo]", seen)
	}
}

// 事件方向也一样：未握手前的事件不能进 handler。
func TestServerRejectsEventBeforeHello(t *testing.T) {
	card := wsTestCard(wsTestMethod("evt"))
	srv, url := wsStartServer(t, card)
	var called int32
	srv.Handle("evt", func(context.Context, *Call) error {
		called++
		return nil
	})
	conn := wsDialRaw(t, url)
	ev, err := NewEvent(wsTestWork, AgentRef("client"), ServiceRef(wsTestService), "evt", nil)
	if err != nil {
		t.Fatal(err)
	}
	wsWriteFrame(t, conn, ev)
	waitReasonCounter(t, srv, "sump_dropped_total", "no_hello", 1)
	if called != 0 {
		t.Fatal("未握手的事件进了 handler")
	}
}

// Send（事件）方向的第一帧同样必须是 hello。
func TestClientAutoHandshakesBeforeFirstEvent(t *testing.T) {
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	var seen []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			f, err := Unmarshal(data)
			if err != nil {
				return
			}
			seen = append(seen, f.Type+":"+f.Method)
			if f.Method == "hello" {
				card := testCard()
				card.Name = "peer"
				card.Actor = ServiceRef("peer")
				reply, err := NewReply(f, ServiceRef("peer"), HelloResponse{Card: card})
				if err != nil {
					return
				}
				raw, _ := Marshal(reply)
				_ = ws.WriteMessage(websocket.BinaryMessage, raw)
			}
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ev, err := NewEvent("w1", AgentRef("client"), ServiceRef("peer"), "tick", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(ev); err != nil {
		t.Fatalf("send: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for len(seen) < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(seen) < 2 || seen[0] != "call:hello" || seen[1] != "event:tick" {
		t.Fatalf("线上前两帧 = %v, want [call:hello event:tick]", seen)
	}
}
