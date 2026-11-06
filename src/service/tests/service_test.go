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

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// ---------- 设置中心替身（原生 /spv2 控制面） ----------

type stubCenter struct {
	ln  net.Listener
	srv *http.Server

	mu       sync.Mutex
	regs     []protocol.RegisterPayload
	byes     int
	conns    map[*spv2.Call]struct{}
	reports  []reportRecord
	announce []announceRecord        // 收到的设置回传（report_setting）
	settings protocol.SettingsResult // list_settings 的应答
}

// announceRecord 是中心收到的一条设置回传。
type announceRecord struct {
	From string
	P    protocol.ReportSettingPayload
}

// reportRecord 是中心收到的一条错误上报。
type reportRecord struct {
	From string
	P    protocol.ReportPayload
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln, conns: map[*spv2.Call]struct{}{}}
	card := spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "settings-center", Actor: spv2.ServiceRef("settings-center"),
		Version: "stub-center/0.1",
		Methods: []spv2.Method{
			{Name: "register", Shape: spv2.ShapeCall, Version: 1},
			{Name: "heartbeat", Shape: spv2.ShapeEvent, Version: 1},
			{Name: "report", Shape: spv2.ShapeCall, Version: 1},
			{Name: "report_setting", Shape: spv2.ShapeCall, Version: 1},
			{Name: "list_settings", Shape: spv2.ShapeCall, Version: 1},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	}
	srv, err := spv2.NewServer(card)
	if err != nil {
		t.Fatal(err)
	}
	srv.Handle("register", sc.handleRegister)
	srv.Handle("heartbeat", sc.handleHeartbeat)
	srv.Handle("report", sc.handleReport)
	srv.Handle("report_setting", sc.handleReportSetting)
	srv.Handle("list_settings", sc.handleListSettings)

	mux := http.NewServeMux()
	mux.Handle("/spv2", srv.Handler())
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/spv2" }

func (sc *stubCenter) handleRegister(_ context.Context, call *spv2.Call) error {
	var reg protocol.RegisterPayload
	if err := call.Frame.DecodeBody(&reg); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	sc.mu.Lock()
	sc.regs = append(sc.regs, reg)
	sc.conns[call] = struct{}{}
	sc.mu.Unlock()
	return call.Reply(protocol.RosterPayload{
		Services: []protocol.ServiceCard{{Name: "settings-center", Addr: sc.URL()}},
		Revision: 1,
	})
}

func (sc *stubCenter) handleHeartbeat(_ context.Context, call *spv2.Call) error {
	var hb protocol.HeartbeatPayload
	_ = call.Frame.DecodeBody(&hb)
	if hb.Status == "bye" {
		sc.mu.Lock()
		sc.byes++
		sc.mu.Unlock()
	}
	return nil
}

func (sc *stubCenter) handleReport(_ context.Context, call *spv2.Call) error {
	var p protocol.ReportPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	sc.mu.Lock()
	sc.reports = append(sc.reports, reportRecord{From: call.Frame.From.ID, P: p})
	sc.mu.Unlock()
	return call.Reply(map[string]any{"status": "ok"})
}

func (sc *stubCenter) handleReportSetting(_ context.Context, call *spv2.Call) error {
	var p protocol.ReportSettingPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	sc.mu.Lock()
	sc.announce = append(sc.announce, announceRecord{From: call.Frame.From.ID, P: p})
	sc.mu.Unlock()
	return call.Reply(map[string]any{"status": "ok"})
}

func (sc *stubCenter) handleListSettings(_ context.Context, call *spv2.Call) error {
	sc.mu.Lock()
	res := sc.settings
	sc.mu.Unlock()
	return call.Reply(res)
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

// Announcements 返回收到的设置回传（并发安全快照）。
func (sc *stubCenter) Announcements() []announceRecord {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := make([]announceRecord, len(sc.announce))
	copy(out, sc.announce)
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
	targets := make([]*spv2.Call, 0, len(sc.conns))
	for c := range sc.conns {
		targets = append(targets, c)
	}
	sc.mu.Unlock()
	for _, c := range targets {
		_ = c.SendEvent(typ, payload)
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
	targets := make([]*spv2.Call, 0, len(sc.conns))
	for c := range sc.conns {
		targets = append(targets, c)
	}
	sc.mu.Unlock()
	for _, c := range targets {
		_ = c.Close()
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
	if !strings.HasSuffix(regs[0].Addr, "/spv2") {
		t.Fatalf("注册地址应指向 /spv2: %q", regs[0].Addr)
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
	s.Fire("no-such-svc", "act", map[string]any{"x": 1}, "")
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

// TestShutdownStopsBackgroundLogging：Shutdown 之后后台循环不能再写日志。
// 回归：watchCenter 过去会在关闭连接后先打一条"1s 后重连"；测试 logger 绑 t 时
// 会在测试结束后 panic（batch/tests 的 TestPauseReleasesGlobalSlots 就这么炸的）。
func TestShutdownStopsBackgroundLogging(t *testing.T) {
	sc := startStubCenter(t)
	var lb logBuffer
	s := service.New(service.Config{
		Name:              "svc-quiet",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Logger:            log.New(&lb, "", 0),
	})
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Shutdown()
	// watchCenter 由连接关闭信号唤醒：旧实现会在此后打"1s 后重连"。
	time.Sleep(300 * time.Millisecond)
	if logs := lb.String(); strings.Contains(logs, "重连") {
		t.Fatalf("Shutdown 后仍有重连日志:\n%s", logs)
	}
}
