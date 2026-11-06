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
	"github.com/NouSart/sump/spv2"
)

const defaultMessageBytes = 1 << 20

func startNativeService(t *testing.T, s *Service) *httptest.Server {
	t.Helper()
	if err := s.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(s.spv2Srv.Handler())
	t.Cleanup(server.Close)
	return server
}

func nativeURL(server *httptest.Server) string {
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/spv2"
}

func effectiveLimit(limit int64) int64 {
	if limit > 0 {
		return limit
	}
	return defaultMessageBytes
}

// 补报入站与普通 call 走同一条 /spv2 路径；超限帧必须在分发到 handler 前被拒绝。
func TestMaxMessageBytesInboundReport(t *testing.T) {
	for _, tc := range []struct {
		name   string
		limit  int64
		size   int
		reject bool
	}{
		{name: "normal", limit: 1024, size: 16},
		{name: "oversized", limit: 1024, size: 4096, reject: true},
		{name: "zero_uses_default", size: 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := New(Config{Name: "receiver", MaxMessageBytes: tc.limit, SecretFile: "-"})
			handled := make(chan int, 1)
			s.HandleNative(spv2.Method{Name: "report_result", Shape: spv2.ShapeCall}, func(_ context.Context, call *spv2.Call) error {
				var data string
				if err := call.Frame.DecodeBody(&data); err != nil {
					return err
				}
				handled <- len(data)
				return call.Reply("ok")
			})
			server := startNativeService(t, s)
			if got := s.spv2Srv.Card().Limits.MaxBody; got != effectiveLimit(tc.limit) {
				t.Fatalf("card max_body = %d, want %d", got, effectiveLimit(tc.limit))
			}
			client, err := spv2.Dial(context.Background(), nativeURL(server))
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			frame, err := spv2.NewCall("trace-report", spv2.ServiceRef("worker"), spv2.ServiceRef("receiver"), "report_result", strings.Repeat("x", tc.size))
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err = client.Call(ctx, frame)
			if tc.reject {
				if !errors.Is(err, spv2.ErrConnClosed) {
					t.Fatalf("oversized report: got %v, want ErrConnClosed", err)
				}
				select {
				case <-handled:
					t.Fatal("oversized report reached method handler")
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
		{name: "zero_uses_default", size: 65536},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer := New(Config{Name: "peer", SecretFile: "-"})
			peer.HandleNative(spv2.Method{Name: "read", Shape: spv2.ShapeCall}, func(_ context.Context, call *spv2.Call) error {
				return call.Reply(strings.Repeat("x", tc.size))
			})
			server := startNativeService(t, peer)
			s := New(Config{Name: "caller", MaxMessageBytes: tc.limit, SecretFile: "-"})
			s.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{{Name: "peer", Addr: nativeURL(server)}}})
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			raw, err := s.Call(ctx, "peer", "read", nil, "")
			if tc.reject {
				if !errors.Is(err, spv2.ErrConnClosed) {
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
	// 这个对端在注册响应里塞了一份超限名册：启动必须失败，而不是接受超出自身上限的响应。
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
		req, err := spv2.Unmarshal(data)
		if err != nil {
			return
		}
		resp, err := spv2.NewReply(req, spv2.ServiceRef("settings-center"), protocol.RosterPayload{
			Services: []protocol.ServiceCard{{Name: "large", Description: strings.Repeat("x", 4096)}},
		})
		if err != nil {
			return
		}
		encoded, err := spv2.Marshal(resp)
		if err != nil {
			return
		}
		_ = ws.WriteMessage(websocket.BinaryMessage, encoded)
		_, _, _ = ws.ReadMessage()
	}))
	defer server.Close()
	s := New(Config{
		Name: "limited", Listen: "127.0.0.1:0",
		Center:          "ws" + strings.TrimPrefix(server.URL, "http") + "/spv2",
		MaxMessageBytes: 1024, SecretFile: "-",
	})
	defer s.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.Start(ctx); !errors.Is(err, spv2.ErrConnClosed) {
		t.Fatalf("oversized registration response: got %v, want ErrConnClosed", err)
	}
}
