package tests

// configure 下发测试：set_setting / reset_setting 成功后，设置中心把变更项的生效值
// 推送给所属服务（尽力送达；服务侧可用启动拉取兜底）。

import (
	"context"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/settings-center/server"
)

// stubService 是接收 configure 的最小服务替身：一个 WS 服务端。
type stubService struct {
	ln  net.Listener
	srv *http.Server

	reject atomic.Bool // 为真时对 configure 回 ok=false（模拟服务拒绝）

	mu  sync.Mutex
	got []protocol.ConfigurePayload
}

func startStubService(t *testing.T) *stubService {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	st := &stubService{ln: ln}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil || env.Type != protocol.TypeJump {
				continue
			}
			var jp protocol.JumpPayload
			if err := env.DecodePayload(&jp); err != nil {
				continue
			}
			if jp.Action == "configure" {
				var p protocol.ConfigurePayload
				if err := protocol.DecodeRaw(jp.Input, &p); err == nil {
					st.mu.Lock()
					st.got = append(st.got, p)
					st.mu.Unlock()
				}
			}
			if jp.Action == "configure" && st.reject.Load() {
				resp, err := protocol.NewResponse(env, "stub-svc", protocol.ResponsePayload{OK: false, Error: "configure: 未支持的设置键"})
				if err != nil {
					continue
				}
				if out, err := protocol.Marshal(resp); err == nil {
					_ = ws.WriteMessage(websocket.BinaryMessage, out)
				}
				continue
			}
			resp, err := protocol.NewResponse(env, "stub-svc", protocol.ResponsePayload{OK: true})
			if err != nil {
				continue
			}
			if out, err := protocol.Marshal(resp); err == nil {
				_ = ws.WriteMessage(websocket.BinaryMessage, out)
			}
		}
	})
	st.srv = &http.Server{Handler: mux}
	go func() { _ = st.srv.Serve(ln) }()
	t.Cleanup(func() { _ = st.srv.Close() })
	return st
}

func (s *stubService) URL() string { return "ws://" + s.ln.Addr().String() + "/ws" }

// waitGot 等待至少 n 条 configure（超时失败）。
func (s *stubService) waitGot(t *testing.T, n int, timeout time.Duration) []protocol.ConfigurePayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		s.mu.Lock()
		got := append([]protocol.ConfigurePayload(nil), s.got...)
		s.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 configure 超时（期望 %d 条）", n)
	return nil
}

// TestConfigurePush 验证：set_setting → 收到 configure（覆盖值）；reset_setting → 再收到（回落默认）。
func TestConfigurePush(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	stub := startStubService(t)

	// 用真实地址注册（configure 会被推送回这个地址）。
	c, err := protocol.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg, err := protocol.NewEnvelope(protocol.TypeRegister, "svc-c", "settings-center", "", protocol.RegisterPayload{
		Name: "svc-c", Addr: stub.URL(), Description: "configure 测试服务",
		Settings: []protocol.Setting{{Key: "tool.notify_mode", Default: "each"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("注册失败: %s", rp.Error)
	}

	// set_setting → configure（覆盖值 batch）
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "svc-c", Key: "tool.notify_mode", Value: "batch"}); msg != "" {
		t.Fatalf("set_setting 失败: %s", msg)
	}
	got := stub.waitGot(t, 1, 5*time.Second)
	if got[0].Key != "tool.notify_mode" || got[0].Value != "batch" || !got[0].Overridden || got[0].Default != "each" {
		t.Fatalf("configure（覆盖）内容不符: %+v", got[0])
	}

	// reset_setting → configure（回落默认 each）
	if _, msg := callSettings(t, wsURL, "reset_setting", protocol.ResetSettingPayload{Service: "svc-c", Key: "tool.notify_mode"}); msg != "" {
		t.Fatalf("reset_setting 失败: %s", msg)
	}
	got = stub.waitGot(t, 2, 5*time.Second)
	if got[1].Value != "each" || got[1].Overridden {
		t.Fatalf("configure（重置）内容不符: %+v", got[1])
	}
}

// syncBuffer 是并发安全的日志缓冲（推送发生在后台 goroutine）。
type syncBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestConfigurePushRejected 验证：服务对 configure 回 ok=false（明确拒绝）时，
// 中心把这次推送记为“被拒绝”（不是“已推送”）；set_setting 本身仍成功（尽力送达不阻塞响应）。
func TestConfigurePushRejected(t *testing.T) {
	var buf syncBuffer
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, log.New(&buf, "[center-test] ", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	stub := startStubService(t)
	stub.reject.Store(true)

	c, err := protocol.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg, err := protocol.NewEnvelope(protocol.TypeRegister, "svc-r", "settings-center", "", protocol.RegisterPayload{
		Name: "svc-r", Addr: stub.URL(), Description: "拒绝 configure 的服务",
		Settings: []protocol.Setting{{Key: "tool.notify_mode", Default: "each"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, reg)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil || !rp.OK {
		t.Fatalf("注册失败: %v / %s", err, rp.Error)
	}

	// 中心接受写入（推送失败不影响 set_setting 的成功回执）。
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "svc-r", Key: "tool.notify_mode", Value: "batch"}); msg != "" {
		t.Fatalf("set_setting 应成功（推送为尽力送达）: %s", msg)
	}

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), "被拒绝") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("中心应记录 configure 推送被拒绝；实际日志: %s", buf.String())
}
