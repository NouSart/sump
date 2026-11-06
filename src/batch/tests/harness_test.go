// Package tests 是 batch 的服务内集成测试：最小"设置中心"替身 + 真 batch 实例 + 一个可批量
// 的目标服务 + 一个"调用方"服务（它同时收补报），全部走 /spv2 原生通信。
package tests

import (
	"context"
	"log"
	"net/http/httptest"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/batch/server"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// ---------- 设置中心替身（register 是 call、heartbeat 是 event、roster 是 event）----------

type stubCenter struct {
	srv  *spv2.Server
	http *httptest.Server

	mu    sync.Mutex
	cards map[string]protocol.ServiceCard
	calls map[string]*spv2.Call // 每个注册者的那条 register call（roster 事件沿它推回去）
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	card := spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "settings-center", Actor: spv2.ServiceRef("settings-center"),
		Version: "stub/1", Methods: []spv2.Method{
			{Name: "register", Shape: spv2.ShapeCall},
			{Name: "list_settings", Shape: spv2.ShapeCall},
			{Name: "heartbeat", Shape: spv2.ShapeEvent},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	}
	srv, err := spv2.NewServer(card)
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{srv: srv, cards: map[string]protocol.ServiceCard{}, calls: map[string]*spv2.Call{}}
	srv.Handle("register", sc.handleRegister)
	srv.Handle("list_settings", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(protocol.SettingsResult{})
	})
	// 心跳是事件，没有回执可发；收下即可（连接还活着就是活着）。
	srv.Handle("heartbeat", func(context.Context, *spv2.Call) error { return nil })
	sc.http = httptest.NewServer(srv.Handler())
	t.Cleanup(sc.http.Close)
	sc.cards["settings-center"] = protocol.ServiceCard{
		Name: "settings-center", Addr: sc.URL(), Description: "设置中心替身",
		Provides: []protocol.Provide{{Action: "list_settings"}, {Action: "roster"}},
	}
	return sc
}

func (sc *stubCenter) URL() string {
	return "ws" + strings.TrimPrefix(sc.http.URL, "http") + "/spv2"
}

func (sc *stubCenter) handleRegister(_ context.Context, call *spv2.Call) error {
	var reg protocol.RegisterPayload
	if err := call.Frame.DecodeBody(&reg); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	sc.mu.Lock()
	sc.cards[reg.Name] = reg.Card()
	sc.calls[reg.Name] = call
	roster := sc.snapshotLocked()
	sc.mu.Unlock()
	if err := call.Reply(roster); err != nil {
		return err
	}
	sc.broadcast(roster)
	return nil
}

func (sc *stubCenter) snapshotLocked() protocol.RosterPayload {
	names := make([]string, 0, len(sc.cards))
	for n := range sc.cards {
		names = append(names, n)
	}
	sort.Strings(names)
	services := make([]protocol.ServiceCard, 0, len(names))
	for _, n := range names {
		services = append(services, sc.cards[n])
	}
	return protocol.RosterPayload{Services: services, Revision: 1}
}

func (sc *stubCenter) broadcast(roster protocol.RosterPayload) {
	sc.mu.Lock()
	targets := make([]*spv2.Call, 0, len(sc.calls))
	for _, call := range sc.calls {
		targets = append(targets, call)
	}
	sc.mu.Unlock()
	for _, call := range targets {
		_ = call.SendEvent("roster", roster)
	}
}

// registerService 用一个原生 register call 把替身登进名册（注册后连接即可关闭）。
func registerService(t *testing.T, sc *stubCenter, name, addr string) {
	t.Helper()
	c, err := spv2.Dial(context.Background(), sc.URL())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call, err := spv2.NewCall("w-reg-"+name, spv2.ServiceRef(name), spv2.ServiceRef("settings-center"), "register",
		protocol.RegisterPayload{Name: name, Addr: addr, Description: name + " 替身"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, call, &roster); err != nil {
		t.Fatalf("%s 注册失败: %v", name, err)
	}
}

// ---------- 目标服务：一个可批量的假服务 ----------

type target struct {
	*service.Service
	mu    sync.Mutex
	calls map[string]int
}

func startTarget(t *testing.T, sc *stubCenter) *target {
	t.Helper()
	tg := &target{calls: map[string]int{}}
	tg.Service = service.New(service.Config{
		Name: "worker", Listen: "127.0.0.1:0", Center: sc.URL(), Logger: log.New(testWriter{t}, "", 0),
		Provides: []protocol.Provide{
			{Action: "ok", Tool: true, Batchable: true, Input: "{n}", Output: "{n, doubled}"},
			{Action: "fail", Tool: true, Batchable: true, Input: "{n}", Output: "总是报错"},
			{Action: "slow", Tool: true, Batchable: true, Input: "{ms}", Output: "睡完返回"},
			{Action: "plain", Tool: true}, // 没声明 Batchable
		},
	})
	tg.HandleNative(spv2.Method{Name: "ok"}, func(_ context.Context, call *spv2.Call) error {
		var p struct {
			N int `msgpack:"n"`
		}
		_ = call.Frame.DecodeBody(&p)
		tg.bump("ok")
		return call.Reply(map[string]any{"n": p.N, "doubled": p.N * 2})
	})
	tg.HandleNative(spv2.Method{Name: "fail"}, func(_ context.Context, call *spv2.Call) error {
		tg.bump("fail")
		return call.ReplyError(spv2.CodeInvalidArgument, "目标说这个不行", false, nil)
	})
	tg.HandleNative(spv2.Method{Name: "slow"}, func(ctx context.Context, call *spv2.Call) error {
		var p struct {
			MS int `msgpack:"ms"`
		}
		_ = call.Frame.DecodeBody(&p)
		tg.bump("slow")
		select {
		case <-time.After(time.Duration(p.MS) * time.Millisecond):
		case <-ctx.Done():
			return ctx.Err()
		}
		return call.Reply(map[string]any{"slept": p.MS})
	})
	tg.HandleNative(spv2.Method{Name: "plain"}, func(_ context.Context, call *spv2.Call) error {
		return call.Reply(map[string]any{"ok": true})
	})
	if err := tg.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tg.Shutdown)
	return tg
}

func (tg *target) bump(action string) {
	tg.mu.Lock()
	tg.calls[action]++
	tg.mu.Unlock()
}

func (tg *target) count(action string) int {
	tg.mu.Lock()
	defer tg.mu.Unlock()
	return tg.calls[action]
}

// testWriter 把服务日志接到 t.Log（失败时能看）。
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

// ---------- 调用方替身：直连 batch 的 /spv2 发调用，同时收 report_result ----------

type caller struct {
	name   string
	srv    *spv2.Server
	http   *httptest.Server
	client *spv2.Client // 打 batch /spv2 的连接
}

func startCaller(t *testing.T, sc *stubCenter, name, batchURL string, reports chan protocol.AsyncResultPayload) *caller {
	t.Helper()
	c := &caller{name: name}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: name, Actor: spv2.ServiceRef(name),
		Version: "stub/1", Methods: []spv2.Method{{Name: "report_result", Shape: spv2.ShapeCall}},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.srv = srv
	srv.Handle("report_result", func(_ context.Context, call *spv2.Call) error {
		var p protocol.AsyncResultPayload
		if err := call.Frame.DecodeBody(&p); err == nil && reports != nil {
			select {
			case reports <- p:
			default:
			}
		}
		return call.Reply(map[string]string{"status": "accepted"})
	})
	c.http = httptest.NewServer(srv.Handler())
	t.Cleanup(c.http.Close)
	registerService(t, sc, name, c.URL())
	c.client = dial(t, batchURL)
	return c
}

func (c *caller) URL() string {
	return "ws" + strings.TrimPrefix(c.http.URL, "http") + "/spv2"
}

// call 以该调用方的名义对 batch 发起一次原生 call。
func (c *caller) call(t *testing.T, action string, in any) ([]byte, error) {
	t.Helper()
	f, err := spv2.NewCall("trace-1", spv2.ServiceRef(c.name), spv2.ServiceRef("batch"), action, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.client.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// close 断开打 batch 的连接并关掉补报接收端（模拟调用方下线）。
func (c *caller) close() {
	_ = c.client.Close()
	c.http.Close()
}

// dial 建立一条到 /spv2 的连接。
func dial(t *testing.T, url string) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// ---------- 夹具 ----------

type fixture struct {
	batch   *server.Server
	target  *target
	center  *stubCenter
	probe   *caller
	reports chan protocol.AsyncResultPayload
}

func startFixture(t *testing.T, over func(*server.Config)) *fixture {
	t.Helper()
	sc := startStubCenter(t)
	tg := startTarget(t, sc)
	cfg := server.Config{
		Name: "batch", Listen: "127.0.0.1:0", Center: sc.URL(),
		DBPath: t.TempDir() + "/batch.db",
	}
	if over != nil {
		over(&cfg)
	}
	b := server.New(cfg, log.New(testWriter{t}, "[batch] ", 0))
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(b.Shutdown)

	f := &fixture{batch: b, target: tg, center: sc, reports: make(chan protocol.AsyncResultPayload, 64)}
	f.probe = startCaller(t, sc, "probe", b.WsURL(), f.reports)
	return f
}

// next 等一条补报。
func (f *fixture) next(t *testing.T, within time.Duration) protocol.AsyncResultPayload {
	t.Helper()
	select {
	case p := <-f.reports:
		return p
	case <-time.After(within):
		t.Fatalf("%s 内没有收到补报", within)
		return protocol.AsyncResultPayload{}
	}
}

// quiet 确认在窗口内没有更多补报。
func (f *fixture) quiet(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case p := <-f.reports:
		t.Fatalf("不该再收到补报，却收到: phase=%s text=%q", p.Phase, p.Text)
	case <-time.After(within):
	}
}

// ---------- 调用助手 ----------

func mustRaw(v any) msgpack.RawMessage {
	raw, err := protocol.EncodePayload(v)
	if err != nil {
		panic(err)
	}
	return raw
}

func items(n int) []protocol.BatchItem {
	out := make([]protocol.BatchItem, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, protocol.BatchItem{Input: mustRaw(map[string]any{"n": i})})
	}
	return out
}

func (f *fixture) callRun(t *testing.T, p protocol.BatchRunPayload) (protocol.BatchRunResult, error) {
	t.Helper()
	raw, err := f.probe.call(t, "run", p)
	if err != nil {
		return protocol.BatchRunResult{}, err
	}
	var res protocol.BatchRunResult
	if err := protocol.DecodeRaw(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

func (f *fixture) mustRun(t *testing.T, p protocol.BatchRunPayload) protocol.BatchRunResult {
	t.Helper()
	res, err := f.callRun(t, p)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (f *fixture) call(t *testing.T, action string, in any) ([]byte, error) {
	t.Helper()
	return f.probe.call(t, action, in)
}

func decodeInto[T any](t *testing.T, raw []byte) T {
	t.Helper()
	var out T
	if err := protocol.DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func mustCall[T any](f *fixture, t *testing.T, action string, in any) T {
	t.Helper()
	raw, err := f.call(t, action, in)
	if err != nil {
		t.Fatal(err)
	}
	return decodeInto[T](t, raw)
}

func (f *fixture) status(t *testing.T, id string) protocol.BatchStatusResult {
	t.Helper()
	return mustCall[protocol.BatchStatusResult](f, t, "status", protocol.BatchStatusPayload{ItemID: id})
}

func (f *fixture) waitPhase(t *testing.T, id, want string, within time.Duration) protocol.BatchStatusResult {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		s := f.status(t, id)
		if s.Phase == want {
			return s
		}
		time.Sleep(20 * time.Millisecond)
	}
	s := f.status(t, id)
	t.Fatalf("%s 内没到 phase=%s，当前 %s（ok=%d failed=%d pending=%d）", within, want, s.Phase, s.Ok, s.Failed, s.Pending)
	return s
}

func (f *fixture) control(t *testing.T, p protocol.BatchControlPayload) protocol.BatchControlResult {
	t.Helper()
	return mustCall[protocol.BatchControlResult](f, t, "control", p)
}

func contains(s, sub string) bool { return strings.Contains(s, sub) }

func countOccurrences(s, sub string) int { return strings.Count(s, sub) }
