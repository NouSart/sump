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

// A real peer fragments its response into small frames. The limit is on the
// whole message, so splitting an oversized result must not bypass it.
func TestClientMaxMessageBytes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		size   int
		legacy bool
		reject bool
	}{
		{name: "normal", limit: 1024, size: 16},
		{name: "oversized_fragmented", limit: 1024, size: 4096, reject: true},
		{name: "zero_is_unlimited", size: 65536},
		{name: "legacy_dial_is_unlimited", size: 65536, legacy: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peerClosed := make(chan error, 1)
			u := websocket.Upgrader{WriteBufferSize: 256}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := u.Upgrade(w, r, nil)
				if err != nil {
					peerClosed <- err
					return
				}
				defer ws.Close()
				_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, data, err := ws.ReadMessage()
				if err != nil {
					peerClosed <- err
					return
				}
				req, err := Unmarshal(data)
				if err != nil {
					peerClosed <- err
					return
				}
				raw, _ := EncodePayload(strings.Repeat("x", tc.size))
				resp, _ := NewResponse(req, "peer", ResponsePayload{OK: true, Data: raw})
				encoded, _ := Marshal(resp)
				writer, err := ws.NextWriter(websocket.BinaryMessage)
				if err == nil {
					_, _ = writer.Write(encoded)
					_ = writer.Close()
				}
				_, _, err = ws.ReadMessage()
				peerClosed <- err
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			addr := "ws" + strings.TrimPrefix(server.URL, "http")
			var client *Client
			var err error
			if tc.legacy {
				client, err = Dial(addr, nil)
			} else {
				client, err = DialContextWithOptions(ctx, addr, nil, DialOptions{MaxMessageBytes: tc.limit})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			req, _ := NewEnvelope(TypeJump, "caller", "peer", "", JumpPayload{Action: "read"})
			resp, err := client.Call(ctx, req)
			if tc.reject {
				if !errors.Is(err, ErrConnClosed) {
					t.Fatalf("oversized response: got %v, want ErrConnClosed", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var result ResponsePayload
				if err := resp.DecodePayload(&result); err != nil {
					t.Fatal(err)
				}
				var value string
				if err := DecodeRaw(result.Data, &value); err != nil || len(value) != tc.size {
					t.Fatalf("response length = %d, want %d; decode error: %v", len(value), tc.size, err)
				}
			}
			_ = client.Close()
			select {
			case err := <-peerClosed:
				if tc.reject && !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
					t.Fatalf("peer close = %v, want close code 1009", err)
				}
			case <-ctx.Done():
				t.Fatal("peer did not observe connection close")
			}
		})
	}
}
