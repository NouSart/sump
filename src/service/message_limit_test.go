package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
)

// Async report_result uses the same inbound WS path as ordinary actions. An
// oversized report must be rejected before dispatch to the action handler.
func TestMaxMessageBytesInboundReport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		size   int
		reject bool
	}{
		{name: "normal", limit: 1024, size: 16},
		{name: "oversized", limit: 1024, size: 4096, reject: true},
		{name: "zero_is_unlimited", size: 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Name: "receiver", MaxMessageBytes: tc.limit})
			handled := make(chan int, 1)
			s.Handle("report_result", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
				var data string
				if err := protocol.DecodeRaw(in.Input, &data); err != nil {
					return nil, err
				}
				handled <- len(data)
				return "ok", nil
			})
			server := httptest.NewServer(http.HandlerFunc(s.handleWS))
			defer server.Close()
			client, err := protocol.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			raw, _ := protocol.EncodePayload(strings.Repeat("x", tc.size))
			req, _ := protocol.NewEnvelope(protocol.TypeJump, "worker", "receiver", "", protocol.JumpPayload{Action: "report_result", Input: raw})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = client.Call(ctx, req)
			if tc.reject {
				if !errors.Is(err, protocol.ErrConnClosed) {
					t.Fatalf("oversized report: got %v, want ErrConnClosed", err)
				}
				select {
				case <-handled:
					t.Fatal("oversized report reached action handler")
				default:
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				select {
				case got := <-handled:
					if got != tc.size {
						t.Fatalf("handler received %d bytes, want %d", got, tc.size)
					}
				case <-ctx.Done():
					t.Fatal("handler did not receive report")
				}
			}
		})
	}
}

func TestMaxMessageBytesOutboundCall(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		size   int
		reject bool
	}{
		{name: "normal", limit: 1024, size: 16},
		{name: "oversized", limit: 1024, size: 4096, reject: true},
		{name: "zero_is_unlimited", size: 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := New(Config{Name: "peer"})
			peer.Handle("read", func(context.Context, protocol.Envelope, protocol.JumpPayload) (any, error) {
				return strings.Repeat("x", tc.size), nil
			})
			server := httptest.NewServer(http.HandlerFunc(peer.handleWS))
			defer server.Close()
			s := New(Config{Name: "caller", MaxMessageBytes: tc.limit})
			s.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{{Name: "peer", Addr: "ws" + strings.TrimPrefix(server.URL, "http")}}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, err := s.Call(ctx, "peer", "read", nil, "", "caller")
			if tc.reject {
				if !errors.Is(err, protocol.ErrConnClosed) {
					t.Fatalf("oversized response: got %v, want ErrConnClosed", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				var value string
				if err := protocol.DecodeRaw(raw, &value); err != nil || len(value) != tc.size {
					t.Fatalf("response length = %d, want %d; decode error: %v", len(value), tc.size, err)
				}
			}
		})
	}
}

func TestMaxMessageBytesCenterRegistration(t *testing.T) {
	// This peer sends a valid, oversized roster response to registration.
	// Startup must fail rather than accepting a response outside its own limit.
	u := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		_ = ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		req, err := protocol.Unmarshal(data)
		if err != nil {
			return
		}
		raw, _ := protocol.EncodePayload(protocol.RosterPayload{Services: []protocol.ServiceCard{{Name: "large", Description: strings.Repeat("x", 4096)}}})
		resp, _ := protocol.NewResponse(req, "settings-center", protocol.ResponsePayload{OK: true, Data: raw})
		encoded, _ := protocol.Marshal(resp)
		_ = ws.WriteMessage(websocket.BinaryMessage, encoded)
		_, _, _ = ws.ReadMessage()
	}))
	defer server.Close()
	s := New(Config{Name: "limited", Listen: "127.0.0.1:0", Center: "ws" + strings.TrimPrefix(server.URL, "http"), MaxMessageBytes: 1024, SecretFile: "-"})
	defer s.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Start(ctx); !errors.Is(err, protocol.ErrConnClosed) {
		t.Fatalf("oversized registration response: got %v, want ErrConnClosed", err)
	}
}
