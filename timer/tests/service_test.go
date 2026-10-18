// Package tests 是 timer 的服务内集成测试：最小“设置中心”替身 + 真 timer 实例 + 真调用方替身，
// 受理、到点补报、撤销都走真实 WS（线上协议，不假）。
//
// 重点验「不阻塞」：一条挂着的长计时，既不该拖住后续受理（回执延迟），
// 也不该拖住后续到点（短计时的补报先到）。
package tests

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/timer/server"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// ---------- 设置中心的最小替身（只做 timer 需要的那点事：注册、名册、吸收心跳）----------

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
	decl  map[string][]protocol.Setting // 服务 → 注册时声明的设置项（断言名片用）
}

// DeclaredSettings 返回某服务注册时声明的设置项（并发安全快照）。
func (sc *stubCenter) DeclaredSettings(name string) []protocol.Setting {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.Setting(nil), sc.decl[name]...)
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
			if sc.decl == nil {
				sc.decl = map[string][]protocol.Setting{}
			}
			sc.decl[reg.Name] = reg.Settings
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

// ---------- 调用方替身：接补报 ----------

type arrived struct {
	p  protocol.AsyncResultPayload
	at time.Time
}

type caller struct {
	ln   net.Listener
	srv  *http.Server
	got  chan arrived
	seen []arrived // 已到但当前断言还用不上的补报（不丢，按句柄认领）
}

func startCaller(t *testing.T, sc *stubCenter, name string) *caller {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &caller{ln: ln, got: make(chan arrived, 64)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", c.handleWS)
	c.srv = &http.Server{Handler: mux}
	go func() { _ = c.srv.Serve(ln) }()
	t.Cleanup(func() { _ = c.srv.Close() })

	reg, err := protocol.Dial(sc.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, name, "settings-center", "", protocol.RegisterPayload{
		Name: name, Addr: c.URL(), Description: "调用方替身",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := reg.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil || !rp.OK {
		t.Fatalf("调用方替身注册失败: %v / %s", err, rp.Error)
	}
	return c
}

func (c *caller) URL() string { return "ws://" + c.ln.Addr().String() + "/ws" }

func (c *caller) handleWS(w http.ResponseWriter, r *http.Request) {
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
		if jp.Action != "report_result" {
			resp, _ := protocol.NewResponse(env, "caller", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
			writeWS(ws, resp)
			continue
		}
		var p protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(jp.Input, &p); err == nil {
			select {
			case c.got <- arrived{p: p, at: time.Now()}:
			default:
			}
		}
		resp, _ := protocol.NewResponse(env, "caller", protocol.ResponsePayload{OK: true})
		writeWS(ws, resp)
	}
}

func writeWS(ws *websocket.Conn, env protocol.Envelope) {
	data, err := protocol.Marshal(env)
	if err != nil {
		return
	}
	_ = ws.WriteMessage(websocket.BinaryMessage, data)
}

// takeReport 取指定条目（句柄）的指定阶段补报。
// 补报由骨架各自成 goroutine 投递，条目之间的到达顺序没有保证——所以
// 不匹配的补报留着而不丢，断言只认句柄。
func takeReport(t *testing.T, c *caller, phase, itemID string) arrived {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for i, a := range c.seen {
			if a.p.Phase == phase && (itemID == "" || a.p.ItemID == itemID) {
				c.seen = append(c.seen[:i], c.seen[i+1:]...)
				return a
			}
		}
		select {
		case a := <-c.got:
			c.seen = append(c.seen, a)
		case <-deadline:
			t.Fatalf("等待补报超时：phase=%s item=%q；已收到 %v", phase, itemID, phasesOf(c.seen))
		}
	}
}

// noReport 断言在窗口期内没有该句柄的该阶段补报（撤销后不该再到点）。
func noReport(t *testing.T, c *caller, phase, itemID string, window time.Duration) {
	t.Helper()
	deadline := time.After(window)
	for {
		for i, a := range c.seen {
			if a.p.Phase == phase && a.p.ItemID == itemID {
				c.seen = append(c.seen[:i], c.seen[i+1:]...)
				t.Fatalf("不该出现的补报：phase=%s item=%s（%s）", phase, itemID, a.p.Text)
			}
		}
		select {
		case a := <-c.got:
			c.seen = append(c.seen, a)
		case <-deadline:
			return
		}
	}
}

func phasesOf(as []arrived) []string {
	out := make([]string, 0, len(as))
	for _, a := range as {
		out = append(out, fmt.Sprintf("%s/%s", a.p.Phase, a.p.ItemID))
	}
	return out
}

// ---------- 被测服务 ----------

func startTimer(t *testing.T, sc *stubCenter, cfg server.Config) *server.Server {
	t.Helper()
	cfg.Name = "timer"
	cfg.Listen = "127.0.0.1:0"
	cfg.Center = sc.URL()
	cfg.HeartbeatInterval = 100 * time.Millisecond
	logger := log.New(os.Stdout, "[timer-test] ", log.LstdFlags|log.Lmicroseconds)
	s := server.New(cfg, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.StopAll) // 测试结束先停掉在计时器，避免回调打在已关闭的服务上
	t.Cleanup(s.Shutdown)
	return s
}

// request 对 timer 发起一次跳转（from 是调用方替身的名字）。
func request(t *testing.T, c *protocol.Client, from, action string, input any) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, from, "timer", "trace-1", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

func dial(t *testing.T, url string) *protocol.Client {
	t.Helper()
	c, err := protocol.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// sleep 发起一次计时并断言受理回执的形状（受理即回执：phase=queued + 句柄非空）。
// 返回值带上这次调用的往返耗时——它就是「受理没有等」的证据。
func sleep(t *testing.T, c *protocol.Client, from string, input protocol.TimerSleepPayload) (protocol.TimerSleepResult, time.Duration) {
	t.Helper()
	start := time.Now()
	rp := request(t, c, from, "sleep", input)
	elapsed := time.Since(start)
	if !rp.OK {
		t.Fatalf("sleep 未被受理: %s", rp.Error)
	}
	var res protocol.TimerSleepResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.ItemID == "" || res.Phase != "queued" || res.FireAt == "" {
		t.Fatalf("受理回执不符: %+v", res)
	}
	return res, elapsed
}

// ---------- 门户替身：收 timer 推来的快照 ----------

type portalStub struct {
	ln  net.Listener
	srv *http.Server

	mu  sync.Mutex
	got []protocol.TimerStatePayload
}

func startPortalStub(t *testing.T, sc *stubCenter, name string) *portalStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &portalStub{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", p.handleWS)
	p.srv = &http.Server{Handler: mux}
	go func() { _ = p.srv.Serve(ln) }()
	t.Cleanup(func() { _ = p.srv.Close() })

	reg, err := protocol.Dial(sc.URL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, name, "settings-center", "", protocol.RegisterPayload{
		Name: name, Addr: p.URL(), Description: "门户替身",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := reg.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil || !rp.OK {
		t.Fatalf("门户替身注册失败: %v / %s", err, rp.Error)
	}
	return p
}

func (p *portalStub) URL() string { return "ws://" + p.ln.Addr().String() + "/ws" }

func (p *portalStub) handleWS(w http.ResponseWriter, r *http.Request) {
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
		if jp.Action == "timer_state" {
			var st protocol.TimerStatePayload
			if err := protocol.DecodeRaw(jp.Input, &st); err == nil {
				p.mu.Lock()
				p.got = append(p.got, st)
				p.mu.Unlock()
			}
			resp, _ := protocol.NewResponse(env, "portal-stub", protocol.ResponsePayload{OK: true})
			writeWS(ws, resp)
			continue
		}
		resp, _ := protocol.NewResponse(env, "portal-stub", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
		writeWS(ws, resp)
	}
}

// snapCount 返回该门户已收到的快照条数（并发安全）。
func (p *portalStub) snapCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.got)
}

// waitState 等第 n 条快照（n 从 1 数）；到达顺序无保证，但快照带 seq，按 seq 取第 n 小的。
func (p *portalStub) waitState(t *testing.T, n int, timeout time.Duration) protocol.TimerStatePayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := append([]protocol.TimerStatePayload(nil), p.got...)
		p.mu.Unlock()
		if len(got) >= n {
			sort.Slice(got, func(i, j int) bool { return got[i].Seq < got[j].Seq })
			return got[n-1]
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("等待第 %d 条快照超时；已收到 %d 条", n, len(p.got))
	return protocol.TimerStatePayload{}
}

// ---------- 用例 ----------

// TestSleepIsAcceptedImmediately：受理即回执——1.5s 的计时，回执必须立刻回来
// （若处理函数里等/睡，这里会等到 ≥1.5s）。
func TestSleepIsAcceptedImmediately(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	res, elapsed := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 1500, Note: "别等"})
	if elapsed > 500*time.Millisecond {
		t.Fatalf("受理回执花了 %s：受理即回执不该等（受理时不应阻塞）", elapsed)
	}
	if _, err := time.Parse(time.RFC3339, res.FireAt); err != nil {
		t.Fatalf("fire_at 不是 RFC3339: %q", res.FireAt)
	}
}

// TestSleepReportsAtDueTime：到点后调用方收到 done 补报，文本里有时长与备注。
func TestSleepReportsAtDueTime(t *testing.T) {
	sc := startStubCenter(t)
	c := startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	res, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 200, Note: "构建应该好了"})
	a := takeReport(t, c, "done", res.ItemID)
	if !a.p.Final {
		t.Fatalf("到点补报应是终态（final）: %+v", a.p)
	}
	if !strings.Contains(a.p.Text, "200ms") || !strings.Contains(a.p.Text, "构建应该好了") {
		t.Fatalf("补报文本缺时长/备注: %q", a.p.Text)
	}
}

// TestLongTimerDoesNotBlockOthers：挂一条 1.5s 长计时，随后的短计时
//   - 受理回执照旧立刻回来（消息循环没被占住）；
//   - 短计时**先于**长计时到点（到点回调各自跑，不排队）。
func TestLongTimerDoesNotBlockOthers(t *testing.T) {
	sc := startStubCenter(t)
	c := startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	long, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 1500, Note: "长的"})

	const n = 10
	shorts := make([]protocol.TimerSleepResult, 0, n)
	for i := 0; i < n; i++ {
		res, elapsed := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 150})
		if elapsed > 500*time.Millisecond {
			t.Fatalf("第 %d 条短计时的受理回执花了 %s：长计时把受理挡住了", i+1, elapsed)
		}
		shorts = append(shorts, res)
	}
	for i, res := range shorts {
		takeReport(t, c, "done", res.ItemID)
		// 短计时都到点了，长计时（1.5s）此刻必然还没到（前面只花了百毫秒级）。
		if i == 0 {
			for _, a := range c.seen {
				if a.p.ItemID == long.ItemID {
					t.Fatalf("长计时早于短计时到点：%+v", a.p)
				}
			}
		}
	}
	takeReport(t, c, "done", long.ItemID)
}

// TestCancelStopsDueReport：撤销后不再到点，且撤销立刻发一条终态补报
// （调用方的在途记账靠 final 释放，不能只静默删掉）。
func TestCancelStopsDueReport(t *testing.T) {
	sc := startStubCenter(t)
	c := startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	res, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 800, Note: "不要了"})

	start := time.Now()
	rp := request(t, cl, "caller", "cancel", protocol.TimerCancelPayload{ItemID: res.ItemID})
	if !rp.OK {
		t.Fatalf("cancel 被拒: %s", rp.Error)
	}
	var cr protocol.TimerCancelResult
	if err := protocol.DecodeRaw(rp.Data, &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Status != "canceled" {
		t.Fatalf("cancel 回执不符: %+v", cr)
	}
	a := takeReport(t, c, "canceled", res.ItemID)
	if !a.p.Final {
		t.Fatalf("撤销补报应是终态: %+v", a.p)
	}
	if late := time.Since(start); late > 500*time.Millisecond {
		t.Fatalf("撤销补报花了 %s：应立刻发", late)
	}
	noReport(t, c, "done", res.ItemID, 1200*time.Millisecond)

	// 再撤一次：已不在表里，如实回 unknown（不假装又撤销了一次）。
	rp = request(t, cl, "caller", "cancel", protocol.TimerCancelPayload{ItemID: res.ItemID})
	var again protocol.TimerCancelResult
	if err := protocol.DecodeRaw(rp.Data, &again); err != nil {
		t.Fatal(err)
	}
	if again.Status != "unknown" {
		t.Fatalf("重复撤销应回 unknown: %+v", again)
	}
}

// TestGuards：时长下限 / 上限 / 在计时器上限三条护栏都必须明确拒绝（不静默截断）。
func TestGuards(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{MaxDuration: time.Second, MaxPending: 2})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	if rp := request(t, cl, "caller", "sleep", protocol.TimerSleepPayload{MS: 50}); rp.OK || !strings.Contains(rp.Error, "至少") {
		t.Fatalf("低于下限应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := request(t, cl, "caller", "sleep", protocol.TimerSleepPayload{MS: 2000}); rp.OK || !strings.Contains(rp.Error, "上限") {
		t.Fatalf("高于上限应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := request(t, cl, "caller", "sleep", protocol.TimerSleepPayload{}); rp.OK {
		t.Fatal("缺 ms 应被拒")
	}
	// 占满两条后，第三条必须被拒（不排队）。
	r1, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 900})
	r2, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 900})
	if rp := request(t, cl, "caller", "sleep", protocol.TimerSleepPayload{MS: 900}); rp.OK || !strings.Contains(rp.Error, "已满") {
		t.Fatalf("满员应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
	// 腾出一条后应能再受理。
	rp := request(t, cl, "caller", "cancel", protocol.TimerCancelPayload{ItemID: r1.ItemID})
	if !rp.OK {
		t.Fatalf("cancel 被拒: %s", rp.Error)
	}
	sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 900})
	_ = r2
}

// TestNoteClipped：超长 note 截断而不是让整条计时失败（截断在字符边界上）。
func TestNoteClipped(t *testing.T) {
	sc := startStubCenter(t)
	c := startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	note := strings.Repeat("备", 250)
	res, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 150, Note: note})
	a := takeReport(t, c, "done", res.ItemID)
	if !strings.Contains(a.p.Text, strings.Repeat("备", 200)+"…") {
		t.Fatalf("超长 note 应被截断（保留前 200 个字符并以省略号收尾）: %q", a.p.Text)
	}
	if strings.Contains(a.p.Text, strings.Repeat("备", 201)) {
		t.Fatalf("截断后不该出现第 201 个字符")
	}
}

// TestPortalGetsSnapshots：推给门户的是**快照**而不是事件——
// 受理一条计时 → 一条含该计时的快照（boss 原样带回）；到点后 → 补一条空快照
// （少了这条，门户那边的倒计时会一直挂着）；撤销同理。
func TestPortalGetsSnapshots(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	portal := startPortalStub(t, sc, "portal-stub")
	startTimer(t, sc, server.Config{Portal: "portal-stub"})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	// 带 boss 的受理：门户靠它认领该给谁展示。
	raw, err := protocol.EncodePayload(protocol.TimerSleepPayload{MS: 200, Note: "看倒计时"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "caller", "timer", "trace-9", protocol.JumpPayload{Action: "sleep", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	env.Boss = "web:private:owner"
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := cl.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil || !rp.OK {
		t.Fatalf("sleep 未被受理: %v / %s", err, rp.Error)
	}
	var res protocol.TimerSleepResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}

	first := portal.waitState(t, 1, 5*time.Second)
	if first.Boss != "web:private:owner" {
		t.Fatalf("快照的 boss 没带回: %+v", first)
	}
	if len(first.Timers) != 1 || first.Timers[0].ItemID != res.ItemID || first.Timers[0].Note != "看倒计时" {
		t.Fatalf("受理后的快照不符: %+v", first.Timers)
	}
	if _, err := time.Parse(time.RFC3339, first.Timers[0].FireAt); err != nil {
		t.Fatalf("快照的 fire_at 不是 RFC3339: %q", first.Timers[0].FireAt)
	}

	// 到点 → 必须补一条空快照。
	second := portal.waitState(t, 2, 5*time.Second)
	if len(second.Timers) != 0 {
		t.Fatalf("到点后应推空快照（否则门户倒计时不清）: %+v", second.Timers)
	}
	if second.Seq <= first.Seq {
		t.Fatalf("快照 seq 必须单调递增: %d → %d", first.Seq, second.Seq)
	}
}

// TestPortalSeesCancel：撤销也推空快照（倒计时同样要立刻消失）。
func TestPortalSeesCancel(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	portal := startPortalStub(t, sc, "portal-stub")
	startTimer(t, sc, server.Config{Portal: "portal-stub"})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	res, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 5000})
	if got := portal.waitState(t, 1, 5*time.Second); len(got.Timers) != 1 {
		t.Fatalf("受理后应有 1 条计时: %+v", got.Timers)
	}
	rp := request(t, cl, "caller", "cancel", protocol.TimerCancelPayload{ItemID: res.ItemID})
	if !rp.OK {
		t.Fatalf("cancel 被拒: %s", rp.Error)
	}
	if got := portal.waitState(t, 2, 5*time.Second); len(got.Timers) != 0 {
		t.Fatalf("撤销后应推空快照: %+v", got.Timers)
	}
}

// TestList：门户冷启动拉一次（之后的变动靠推）。
func TestList(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	a, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 3000})
	b, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 1000})

	rp := request(t, cl, "caller", "list", map[string]any{})
	if !rp.OK {
		t.Fatalf("list 被拒: %s", rp.Error)
	}
	var lr protocol.TimerListResult
	if err := protocol.DecodeRaw(rp.Data, &lr); err != nil {
		t.Fatal(err)
	}
	if len(lr.Timers) != 2 {
		t.Fatalf("应列出 2 条: %+v", lr.Timers)
	}
	// 按到点时刻升序：1s 的那条在前。
	if lr.Timers[0].ItemID != b.ItemID || lr.Timers[1].ItemID != a.ItemID {
		t.Fatalf("list 未按到点时刻排序: %+v", lr.Timers)
	}
	if lr.Seq <= 0 {
		t.Fatalf("list 应带快照序号: %+v", lr)
	}
}

// TestCeilingSetting：设置项 max_ms 调不过 23h 天花板（再长补报会被调用方忽略），
// 也不接受低于下限的值。走的是中心下发同一条路：configure 动作。
func TestCeilingSetting(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	setMax := func(ms string) protocol.ResponsePayload {
		return request(t, cl, "caller", "configure", protocol.ConfigurePayload{Key: "max_ms", Value: ms})
	}
	if rp := setMax("82800000"); !rp.OK { // 23h
		t.Fatalf("23h 应被接受: %s", rp.Error)
	}
	if rp := setMax("86400000"); rp.OK || !strings.Contains(rp.Error, "天花板") { // 24h
		t.Fatalf("24h 应被天花板拒绝: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := setMax("50"); rp.OK {
		t.Fatal("低于下限应被拒")
	}
	if rp := setMax("7200000"); !rp.OK { // 2h
		t.Fatalf("2h 应被接受: %s", rp.Error)
	}

	// 生效的是最后一次成功值：2h 受理、2h+1ms 拒绝（旧值 1h 已被覆盖）。
	if rp := request(t, cl, "caller", "sleep", protocol.TimerSleepPayload{MS: 7200000}); !rp.OK {
		t.Fatalf("2h 计时应被受理: %s", rp.Error)
	}
	if rp := request(t, cl, "caller", "sleep", protocol.TimerSleepPayload{MS: 7200001}); rp.OK {
		t.Fatal("超过新上限应被拒")
	}
}

// listenAddr 从替身中心的名册里取 timer 的 WS 地址（去掉 ws:// 前缀，**保留 /ws 路径**——
// 骨架的 WS 挂在 /ws 上，连根路径会 bad handshake）。调用方用 "ws://" + 它 拼回完整地址。
func listenAddr(t *testing.T, sc *stubCenter) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sc.mu.Lock()
		card, ok := sc.cards["timer"]
		sc.mu.Unlock()
		if ok {
			return strings.TrimPrefix(card.Addr, "ws://")
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timer 未在 5 秒内注册")
	return ""
}
