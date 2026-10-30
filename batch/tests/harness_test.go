// Package tests 是 batch 的服务内集成测试：最小"设置中心"替身 + 真 batch 实例 + 一个可批量
// 的目标服务 + 一个"调用方"服务（它同时收补报），走真实 WS 通信。
package tests

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/batch/server"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

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

	mu    sync.Mutex
	cards map[string]protocol.ServiceCard
	conns map[*stubConn]struct{}
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln, cards: map[string]protocol.ServiceCard{}, conns: map[*stubConn]struct{}{}}
	sc.cards["settings-center"] = protocol.ServiceCard{
		Name: "settings-center", Addr: sc.URL(), Description: "设置中心替身",
		Provides: []protocol.Provide{{Action: "list_settings"}, {Action: "roster"}},
	}
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
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			sc.mu.Lock()
			delete(sc.conns, c)
			sc.mu.Unlock()
			_ = ws.Close()
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
			sc.cards[reg.Name] = reg.Card()
			sc.conns[c] = struct{}{}
			roster := sc.snapshotLocked()
			sc.mu.Unlock()
			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: respData})
			c.send(resp)
			sc.broadcast(roster)
		case protocol.TypeJump:
			var jp protocol.JumpPayload
			if err := env.DecodePayload(&jp); err != nil {
				continue
			}
			if jp.Action == "list_settings" {
				data, _ := protocol.EncodePayload(protocol.SettingsResult{})
				resp, _ := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: data})
				c.send(resp)
			}
		case protocol.TypeHeartbeat:
			// 忽略
		}
	}
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
	targets := make([]*stubConn, 0, len(sc.conns))
	for c := range sc.conns {
		targets = append(targets, c)
	}
	sc.mu.Unlock()
	for _, c := range targets {
		env, err := protocol.NewEnvelope(protocol.TypeRoster, "settings-center", "", "", roster)
		if err == nil {
			c.send(env)
		}
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
	tg.Handle("ok", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var p struct {
			N int `msgpack:"n"`
		}
		_ = protocol.DecodeRaw(in.Input, &p)
		tg.bump("ok")
		return map[string]any{"n": p.N, "doubled": p.N * 2}, nil
	})
	tg.Handle("fail", func(_ context.Context, _ protocol.Envelope, _ protocol.JumpPayload) (any, error) {
		tg.bump("fail")
		return nil, errors.New("目标说这个不行")
	})
	tg.Handle("slow", func(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var p struct {
			MS int `msgpack:"ms"`
		}
		_ = protocol.DecodeRaw(in.Input, &p)
		tg.bump("slow")
		select {
		case <-time.After(time.Duration(p.MS) * time.Millisecond):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return map[string]any{"slept": p.MS}, nil
	})
	tg.Handle("plain", func(_ context.Context, _ protocol.Envelope, _ protocol.JumpPayload) (any, error) {
		return map[string]any{"ok": true}, nil
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

// ---------- 夹具 ----------

type fixture struct {
	batch   *server.Server
	target  *target
	center  *stubCenter
	probe   *service.Service
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
	f.probe = service.New(service.Config{
		Name: "probe", Listen: "127.0.0.1:0", Center: sc.URL(), Logger: log.New(testWriter{t}, "[probe] ", 0),
	})
	f.probe.Handle("report_result", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var p protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(in.Input, &p); err != nil {
			return nil, err
		}
		select {
		case f.reports <- p:
		default:
		}
		return map[string]string{"status": "accepted"}, nil
	})
	if err := f.probe.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.probe.Shutdown)
	waitRoster(t, f.probe, "batch")
	waitRoster(t, f.probe, "worker")
	return f
}

func waitRoster(t *testing.T, s *service.Service, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range s.Roster() {
			if c.Name == name {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("名册里一直没出现 %s", name)
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	raw, err := f.probe.Call(ctx, "batch", "run", p, "", "boss-1")
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
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return f.probe.Call(ctx, "batch", action, in, "", "boss-1")
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
