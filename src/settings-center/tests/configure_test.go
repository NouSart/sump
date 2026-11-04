package tests

// configure 下发测试：注册即全量下发、变更单键下发、结果以 settings 事件广播
// （受理回执不等待送达）；覆盖不可达服务（未送达）与服务拒绝两类失败。

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

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
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
				resp, err := protocol.NewResponse(env, "stub-svc", protocol.ResponsePayload{OK: false, Error: `configure: 未声明的设置项 "tool.notify_mode"`})
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

// eventRecorder 收集中心广播的 settings 事件（挂在注册连接上）。
type eventRecorder struct {
	mu     sync.Mutex
	events []protocol.SettingsEventPayload
}

func (ev *eventRecorder) onEvent(env protocol.Envelope) {
	if env.Type != protocol.TypeSettings {
		return
	}
	var p protocol.SettingsEventPayload
	if err := env.DecodePayload(&p); err != nil {
		return
	}
	ev.mu.Lock()
	ev.events = append(ev.events, p)
	ev.mu.Unlock()
}

func (ev *eventRecorder) wait(t *testing.T, n int, timeout time.Duration) []protocol.SettingsEventPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev.mu.Lock()
		got := append([]protocol.SettingsEventPayload(nil), ev.events...)
		ev.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("等待 settings 事件超时（期望 %d 条）", n)
	return nil
}

// registerService 注册一个服务并收集中心广播的 settings 事件。
func registerService(t *testing.T, wsURL, name, addr string, settings []protocol.Setting) (*protocol.Client, *eventRecorder) {
	t.Helper()
	events := &eventRecorder{}
	c, err := protocol.Dial(wsURL, events.onEvent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	reg, err := protocol.NewEnvelope(protocol.TypeRegister, name, "settings-center", "", protocol.RegisterPayload{
		Name: name, Addr: addr, Description: "事件观察测试服务", Settings: settings,
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
	return c, events
}

// TestConfigurePush 验证：注册即全量下发（预设值）；set_setting / reset_setting 单键下发；
// 每一步的送达结果都以 settings 事件广播（cause 区分）。
func TestConfigurePush(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	stub := startStubService(t)
	_, events := registerService(t, wsURL, "svc-c", stub.URL(), []protocol.Setting{{Key: "tool.notify_mode", Default: "each"}})

	// 注册即下发：预设值（未覆盖）。
	got := stub.waitGot(t, 1, 5*time.Second)
	if got[0].Key != "tool.notify_mode" || got[0].Value != "each" || got[0].Overridden {
		t.Fatalf("注册下发（预设）内容不符: %+v", got[0])
	}
	ev := events.wait(t, 1, 5*time.Second)
	if ev[0].Cause != "register" || ev[0].Service != "svc-c" || len(ev[0].Results) != 1 || !ev[0].Results[0].OK || ev[0].Results[0].Value != "each" {
		t.Fatalf("注册事件不符: %+v", ev[0])
	}

	// set_setting → configure（覆盖值 batch）+ 事件（cause=set_setting）
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "svc-c", Key: "tool.notify_mode", Value: "batch"}); msg != "" {
		t.Fatalf("set_setting 失败: %s", msg)
	}
	got = stub.waitGot(t, 2, 5*time.Second)
	if got[1].Key != "tool.notify_mode" || got[1].Value != "batch" || !got[1].Overridden || got[1].Default != "each" {
		t.Fatalf("configure（覆盖）内容不符: %+v", got[1])
	}
	ev = events.wait(t, 2, 5*time.Second)
	if ev[1].Cause != "set_setting" || len(ev[1].Results) != 1 || !ev[1].Results[0].OK || ev[1].Results[0].Value != "batch" {
		t.Fatalf("设置变更事件不符: %+v", ev[1])
	}

	// reset_setting → configure（回落预设 each）
	if _, msg := callSettings(t, wsURL, "reset_setting", protocol.ResetSettingPayload{Service: "svc-c", Key: "tool.notify_mode"}); msg != "" {
		t.Fatalf("reset_setting 失败: %s", msg)
	}
	got = stub.waitGot(t, 3, 5*time.Second)
	if got[2].Value != "each" || got[2].Overridden {
		t.Fatalf("configure（重置）内容不符: %+v", got[2])
	}
	ev = events.wait(t, 3, 5*time.Second)
	if ev[2].Cause != "reset_setting" || !ev[2].Results[0].OK || ev[2].Results[0].Value != "each" {
		t.Fatalf("重置事件不符: %+v", ev[2])
	}
}

// TestRegisterPushAll 验证：重注册（服务重启）时中心全量下发**生效值**（覆盖 ?? 预设）。
func TestRegisterPushAll(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	stub := startStubService(t)
	settings := []protocol.Setting{
		{Key: "tool.notify_mode", Default: "each"},
		{Key: "tool.timeout_ms", Default: "60000"},
	}
	c1, _ := registerService(t, wsURL, "svc-p", stub.URL(), settings)
	got := stub.waitGot(t, 2, 5*time.Second)
	if got[0].Key != "tool.notify_mode" || got[0].Value != "each" || got[1].Key != "tool.timeout_ms" || got[1].Value != "60000" {
		t.Fatalf("首次注册应全量下发预设值: %+v", got)
	}

	// 写覆盖：mode=batch（触发单键下发）。
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "svc-p", Key: "tool.notify_mode", Value: "batch"}); msg != "" {
		t.Fatalf("set_setting 失败: %s", msg)
	}
	stub.waitGot(t, 3, 5*time.Second)

	// 模拟重启：断开旧连接、重新注册（名片与声明都不变）→ 仍应全量下发生效值。
	c1.Close()
	registerService(t, wsURL, "svc-p", stub.URL(), settings)
	got = stub.waitGot(t, 5, 5*time.Second)
	if got[3].Key != "tool.notify_mode" || got[3].Value != "batch" || !got[3].Overridden {
		t.Fatalf("重注册应下发覆盖值: %+v", got[3])
	}
	if got[4].Key != "tool.timeout_ms" || got[4].Value != "60000" || got[4].Overridden {
		t.Fatalf("重注册应下发预设值: %+v", got[4])
	}
}

// TestSelfSettings 验证：设置中心自身也是服务——两项声明、非法值下发失败（事件 ok=false），
// 合法值本地生效（事件 ok=true，list_settings 显示覆盖）。
func TestSelfSettings(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	// 未注册的连接收不到广播：用注册服务作为观察者。
	_, events := registerService(t, wsURL, "svc-obs", "ws://127.0.0.1:1/ws", nil)

	res, msg := callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "settings-center"})
	if msg != "" {
		t.Fatalf("list_settings 失败: %s", msg)
	}
	if len(res.Services) != 1 || len(res.Services[0].Settings) != 4 {
		t.Fatalf("中心应声明四个设置项: %+v", res)
	}
	if res.Services[0].Settings[0].Key != "sweep_ms" || res.Services[0].Settings[0].Value != "5000" {
		t.Fatalf("sweep_ms 预设不符: %+v", res.Services[0].Settings[0])
	}
	if res.Services[0].Settings[1].Key != "heartbeat_fallback_ms" || res.Services[0].Settings[1].Value != "15000" {
		t.Fatalf("heartbeat_fallback_ms 预设不符: %+v", res.Services[0].Settings[1])
	}
	// 后两项是只读项：启动参数带起来的（换它要重启），进面板只为看得见
	for _, i := range []int{2, 3} {
		if it := res.Services[0].Settings[i]; !it.ReadOnly {
			t.Fatalf("第 %d 项应是只读项: %+v", i, it)
		}
	}
	if it := res.Services[0].Settings[2]; it.Key != "addr" || it.Value != "127.0.0.1:0" {
		t.Fatalf("addr 只读项应显示启动值: %+v", it)
	}
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "settings-center", Key: "addr", Value: "0.0.0.0:1"}); msg == "" || !strings.Contains(msg, "只读") {
		t.Fatalf("改中心的只读项应被拒: %s", msg)
	}

	// 非法值：写入成功（受理回执），但本地应用失败 → 事件 ok=false。
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "settings-center", Key: "sweep_ms", Value: "abc"}); msg != "" {
		t.Fatalf("set_setting 本身应成功（应用结果走事件）: %s", msg)
	}
	ev := events.wait(t, 1, 5*time.Second)
	if ev[0].Cause != "set_setting" || ev[0].Service != "settings-center" || len(ev[0].Results) != 1 || ev[0].Results[0].OK || !strings.Contains(ev[0].Results[0].Error, "≥100") {
		t.Fatalf("非法值应回 ok=false 并带原因: %+v", ev[0])
	}

	// 合法值：本地生效。
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "settings-center", Key: "sweep_ms", Value: "250"}); msg != "" {
		t.Fatalf("set_setting 失败: %s", msg)
	}
	ev = events.wait(t, 2, 5*time.Second)
	if !ev[1].Results[0].OK || ev[1].Results[0].Value != "250" {
		t.Fatalf("合法值应回 ok=true: %+v", ev[1])
	}
	res, msg = callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "settings-center"})
	if msg != "" || res.Services[0].Settings[0].Value != "250" || !res.Services[0].Settings[0].Overridden {
		t.Fatalf("中心设置视图应显示覆盖: %+v / %s", res, msg)
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
// 中心把这次下发记为“未生效”（原因是服务的拒绝理由、不重试）；
// set_setting 本身仍成功（受理回执不等待送达结果）。
func TestConfigurePushRejected(t *testing.T) {
	var buf syncBuffer
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, log.New(&buf, "[center-test] ", 0))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	stub := startStubService(t)
	registerService(t, wsURL, "svc-r", stub.URL(), []protocol.Setting{{Key: "tool.notify_mode", Default: "each"}})
	stub.waitGot(t, 1, 5*time.Second) // 注册全量下发先成功（此时未拒绝）

	// 之后服务开始拒绝：中心接受写入（受理回执不等待送达），下发记为未生效。
	stub.reject.Store(true)
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "svc-r", Key: "tool.notify_mode", Value: "batch"}); msg != "" {
		t.Fatalf("set_setting 应成功（受理回执不等待送达）: %s", msg)
	}
	stub.waitGot(t, 2, 5*time.Second)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		logText := buf.String()
		if strings.Contains(logText, "推送未生效") && strings.Contains(logText, "未声明的设置项") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("中心应记录下发未生效（含服务给出的原因）；实际日志: %s", buf.String())
}

// TestSetWhenOffline 验证“受理不等待送达”的边界：目标服务不可达时，
// set 受理回执**立即**返回（不等待下发重试）；下发经有界重试失败后，
// 以 settings 事件回传 ok=false（未送达 + 原因）。
func TestSetWhenOffline(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	// 名片地址指向死端口：中心一切下发都会失败（服务自身连接仍正常）。
	_, events := registerService(t, wsURL, "svc-off", "ws://127.0.0.1:1/ws", []protocol.Setting{{Key: "k", Default: "d"}})

	// 1) 注册下发经重试后失败 → 事件 ok=false（未送达）。
	ev := events.wait(t, 1, 15*time.Second)
	if ev[0].Cause != "register" || len(ev[0].Results) != 1 || ev[0].Results[0].OK || !strings.Contains(ev[0].Results[0].Error, "未送达") {
		t.Fatalf("不可达服务的注册下发应回 ok=false（未送达）: %+v", ev[0])
	}

	// 2) set：受理回执立即返回（远早于下发重试链），视图显示覆盖已落盘。
	start := time.Now()
	res, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{Service: "svc-off", Key: "k", Value: "v"})
	elapsed := time.Since(start)
	if msg != "" {
		t.Fatalf("set_setting 应被受理（不等待送达）: %s", msg)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("受理回执不应等待下发重试，耗时 %v", elapsed)
	}
	if len(res.Services) != 1 || len(res.Services[0].Settings) != 1 || res.Services[0].Settings[0].Value != "v" || !res.Services[0].Settings[0].Overridden {
		t.Fatalf("受理后视图应显示覆盖值: %+v", res)
	}

	// 3) 下发失败 → 事件回传 ok=false（未送达 + 原因）。
	ev = events.wait(t, 2, 15*time.Second)
	if ev[1].Cause != "set_setting" || len(ev[1].Results) != 1 || ev[1].Results[0].OK || !strings.Contains(ev[1].Results[0].Error, "未送达") {
		t.Fatalf("不可达服务的 set 下发应回 ok=false（未送达）: %+v", ev[1])
	}
}
