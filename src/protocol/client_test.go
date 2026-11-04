package protocol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// 连接已经关掉时 Call 报 ErrConnClosed。
//
// 这条守的是错误身份的一致性：Call 有两条失败路径——读循环关闭（早就返回
// ErrConnClosed）和写失败。写失败时 gorilla 给的是"use of closed network
// connection"这类底层错误，原样透出去，调用方就得同时认两种错误才算数。
// 这里用「先关客户端再 Call」把写失败路径钉死，不依赖网络时序。
func TestCallOnClosedClientReportsErrConnClosed(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_, _, _ = ws.ReadMessage()
	}))
	defer server.Close()

	client, err := Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, _ := NewEnvelope(TypeJump, "caller", "peer", "", JumpPayload{Action: "read"})
	if _, err := client.Call(ctx, req); !errors.Is(err, ErrConnClosed) {
		t.Fatalf("已关闭的连接: got %v, want ErrConnClosed", err)
	}
}
