// service 骨架测试：注册（声明心跳）→ 断线自动重连并重新注册 → 关停时告别（bye）。
package tests

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// ---------- 设置中心替身 ----------

type stubConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *stubConn) send(env protocol.Envelope) {
	data, err := protocol.Marshal(env)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.BinaryMessage, data)
}

type stubCenter struct {
	ln  net.Listener
	srv *http.Server

	mu       sync.Mutex
	regs     []protocol.RegisterPayload
	byes     int
	conns    map[*stubConn]struct{}
	reports  []reportRecord
	settings protocol.SettingsResult // list_settings 的应答
}

// reportRecord 是中心收到的一条错误上报。
type reportRecord struct {
	From string
	P    protocol.ReportPayload
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln, conns: map[*stubConn]struct{}{}}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sc.handleWS)
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/ws" }

func (sc *stubCenter) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	sc.mu.Lock()
	sc.conns[c] = struct{}{}
	sc.mu.Unlock()
	defer func() {
		sc.mu.Lock()
		delete(sc.conns, c)
		sc.mu.Unlock()
		_ = ws.Close()
	}()

	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeRegister:
			var reg protocol.RegisterPayload
			if err := env.DecodePayload(&reg); err != nil {
				continue
			}
			sc.mu.Lock()
			sc.regs = append(sc.regs, reg)
			sc.mu.Unlock()
			roster := protocol.RosterPayload{
				Services: []protocol.ServiceCard{{Name: "settings-center", Addr: sc.URL()}},
				Revision: 1,
			}
			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true, Data: respData})
			c.send(resp)
		case protocol.TypeHeartbeat:
			var hb protocol.HeartbeatPayload
			_ = env.DecodePayload(&hb)
			if hb.Status == "bye" {
				sc.mu.Lock()
				sc.byes++
				sc.mu.Unlock()
			}
		case protocol.TypeJump:
			var jp protocol.JumpPayload
			if err := env.DecodePayload(&jp); err != nil {
				continue
			}
			switch jp.Action {
			case "report":
				var p protocol.ReportPayload
				if err := protocol.DecodeRaw(jp.Input, &p); err == nil {
					sc.mu.Lock()
					sc.reports = append(sc.reports, reportRecord{From: env.From, P: p})
					sc.mu.Unlock()
				}
				resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true})
				c.send(resp)
			case "list_settings":
				sc.mu.Lock()
				res := sc.settings
				sc.mu.Unlock()
				data, _ := protocol.EncodePayload(res)
				resp, _ := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: data})
				c.send(resp)
			}
		}
	}
}

// Registers 返回已收到的注册快照。
func (sc *stubCenter) Registers() []protocol.RegisterPayload {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := make([]protocol.RegisterPayload, len(sc.regs))
	copy(out, sc.regs)
	return out
}

// Byes 返回已收到的告别心跳数。
func (sc *stubCenter) Byes() int {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.byes
}

// Reports 返回已收到的上报快照。
func (sc *stubCenter) Reports() []reportRecord {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := make([]reportRecord, len(sc.reports))
	copy(out, sc.reports)
	return out
}

// SetSettings 设置 list_settings 的应答（并发安全）。
func (sc *stubCenter) SetSettings(res protocol.SettingsResult) {
	sc.mu.Lock()
	sc.settings = res
	sc.mu.Unlock()
}

// Broadcast 向所有已连接服务广播一条事件（测试骨架对已知 / 未知事件的处理）。
func (sc *stubCenter) Broadcast(typ string, payload any) {
	sc.mu.Lock()
	targets := make([]*stubConn, 0, len(sc.conns))
	for c := range sc.conns {
		targets = append(targets, c)
	}
	sc.mu.Unlock()
	for _, c := range targets {
		env, err := protocol.NewEnvelope(typ, "settings-center", "", "", payload)
		if err == nil {
			c.send(env)
		}
	}
}

// waitReports 等待至少 n 条上报（超时失败）。
func (sc *stubCenter) waitReports(t *testing.T, n int, timeout time.Duration) []reportRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := sc.Reports(); len(got) >= n {
			return got
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("等待 %d 条上报超时（实际 %d）", n, len(sc.Reports()))
	return nil
}

// DropConnections 模拟链路断开（如设置中心侧关闭全部连接）。
func (sc *stubCenter) DropConnections() {
	sc.mu.Lock()
	targets := make([]*stubConn, 0, len(sc.conns))
	for c := range sc.conns {
		targets = append(targets, c)
	}
	sc.mu.Unlock()
	for _, c := range targets {
		_ = c.ws.Close()
	}
}

// ---------- 用例 ----------

// TestRegisterReconnectBye 验证骨架核心：注册（声明心跳）→ 断线自动重连并重新注册 → 关停时告别。
func TestRegisterReconnectBye(t *testing.T) {
	sc := startStubCenter(t)
	logger := log.New(os.Stdout, "[svc-test] ", log.LstdFlags)

	s := service.New(service.Config{
		Name:              "svc-t",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Description:       "骨架测试服务",
		Provides:          []protocol.Provide{{Action: "echo", Output: "原样返回"}},
		Logger:            logger,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	regs := sc.Registers()
	if len(regs) != 1 {
		t.Fatalf("注册次数 = %d, want 1", len(regs))
	}
	if regs[0].Name != "svc-t" || regs[0].HeartbeatMS != 100 {
		t.Fatalf("注册名片不符: %+v", regs[0])
	}

	// 断线 → 应自动重连并重新注册
	sc.DropConnections()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(sc.Registers()) < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := len(sc.Registers()); got < 2 {
		t.Fatalf("断线后应重新注册，实际注册次数 = %d", got)
	}

	// 关停 → 应发一条 bye（中心据此立即摘除名册）
	s.Shutdown()
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && sc.Byes() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if sc.Byes() == 0 {
		t.Fatal("关停时应发送 bye 心跳")
	}
}

// TestErrorReport 验证错误上报通道：Fire 未送达 → 骨架自动上报（kind=fire）；
// 连发被退避节流（1s 档）；退避窗口过后再次放行。
func TestErrorReport(t *testing.T) {
	sc := startStubCenter(t)
	logger := log.New(os.Stdout, "[svc-test] ", log.LstdFlags)

	s := service.New(service.Config{
		Name:              "svc-r",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Logger:            logger,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	// Fire 到未知服务 → "未送达" → 骨架自动上报。
	s.Fire("no-such-svc", "act", map[string]any{"x": 1}, "", "")
	got := sc.waitReports(t, 1, 5*time.Second)
	if got[0].From != "svc-r" || got[0].P.Kind != "fire" || !strings.Contains(got[0].P.Message, "no-such-svc") {
		t.Fatalf("上报内容不符: %+v", got[0])
	}

	// 防刷：紧跟着的连发应被退避节流（1s 档窗口内）。
	for i := 0; i < 3; i++ {
		s.Report("direct", "连发")
	}
	time.Sleep(400 * time.Millisecond)
	if n := len(sc.Reports()); n != 1 {
		t.Fatalf("退避应节流连发（期望仍 1 条，实际 %d）", n)
	}

	// 退避窗口过后（1s 档 → 2s 档），再次放行。
	deadline := time.Now().Add(3 * time.Second)
	sent := false
	for time.Now().Before(deadline) {
		s.Report("direct", "重试")
		if len(sc.Reports()) >= 2 {
			sent = true
			break
		}
		time.Sleep(200 * time.Millisecond)
	}
	if !sent {
		t.Fatal("退避窗口过后应再次放行上报")
	}
}
