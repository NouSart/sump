package tests

// timer 端到端（真进程）：settings-center + timer 两个真实进程；调用方替身在本进程内以真 WS
// 注册进中心——timer 到点要按名册解析它的地址**直连**回来（与线上路径一致，中心不转发跳转）。
//
// 验四件事：受理即回执（长计时也不等）、到点补报跳回调用方、撤销后不再到点、
// 以及设置热链（经中心改 timer.max_ms 后新上限立即生效）。

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
)

var timerUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// e2eCaller 是调用方替身：像任何服务一样注册进真中心，并收 timer 跳回来的补报。
type e2eCaller struct {
	ln  net.Listener
	srv *http.Server

	mu  sync.Mutex
	got []protocol.AsyncResultPayload
}

func startE2ECaller(t *testing.T, centerURL, name string) *e2eCaller {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &e2eCaller{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", c.handleWS)
	c.srv = &http.Server{Handler: mux}
	go func() { _ = c.srv.Serve(ln) }()
	t.Cleanup(func() { _ = c.srv.Close() })

	// 注册进真中心：timer 到点要靠名册里的地址直连回来。
	reg, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, name, "settings-center", "", protocol.RegisterPayload{
		Name: name, Addr: c.URL(), Description: "端到端调用方替身",
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

func (c *e2eCaller) URL() string { return "ws://" + c.ln.Addr().String() + "/ws" }

func (c *e2eCaller) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := timerUpgrader.Upgrade(w, r, nil)
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
		if jp.Action == "report_result" {
			var p protocol.AsyncResultPayload
			if err := protocol.DecodeRaw(jp.Input, &p); err == nil {
				c.mu.Lock()
				c.got = append(c.got, p)
				c.mu.Unlock()
			}
			resp, _ := protocol.NewResponse(env, "e2e-caller", protocol.ResponsePayload{OK: true})
			writeWS(ws, resp)
			continue
		}
		resp, _ := protocol.NewResponse(env, "e2e-caller", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
		writeWS(ws, resp)
	}
}

// waitReport 等一条指定条目、指定阶段的补报（超时即失败）。
func (c *e2eCaller) waitReport(t *testing.T, phase, item string, timeout time.Duration) protocol.AsyncResultPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p, ok := c.take(phase, item); ok {
			return p
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待补报超时：phase=%s item=%s；已收到 %v", phase, item, c.phases())
	return protocol.AsyncResultPayload{}
}

// absent 在指定窗口内确认没有该条目该阶段的补报（撤销后就该如此）。
func (c *e2eCaller) absent(t *testing.T, phase, item string, window time.Duration) {
	t.Helper()
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		if p, ok := c.take(phase, item); ok {
			t.Fatalf("不该出现的补报：phase=%s item=%s text=%q", phase, item, p.Text)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *e2eCaller) take(phase, item string) (protocol.AsyncResultPayload, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for i, p := range c.got {
		if p.Phase == phase && p.ItemID == item {
			c.got = append(c.got[:i], c.got[i+1:]...)
			return p, true
		}
	}
	return protocol.AsyncResultPayload{}, false
}

func (c *e2eCaller) phases() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.got))
	for _, p := range c.got {
		out = append(out, fmt.Sprintf("%s/%s", p.Phase, p.ItemID))
	}
	return out
}

// callJumpFrom 与 callJump 同（见 e2e_test.go），但可指定信封 from——
// timer 靠 from 才知道到点把补报跳回谁，用错就等于把补报发给了别人。
func callJumpFrom(t *testing.T, c *protocol.Client, from, to, action string, input any, ctx context.Context) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, from, to, "", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
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

// e2ePortal 是门户替身：只收 timer 推来的计时快照（真门户是 node 进程，这里验的是推的链路本身）。
type e2ePortal struct {
	ln  net.Listener
	srv *http.Server

	mu  sync.Mutex
	got []protocol.TimerStatePayload
}

func startE2EPortal(t *testing.T, centerURL, name string) *e2ePortal {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &e2ePortal{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", p.handleWS)
	p.srv = &http.Server{Handler: mux}
	go func() { _ = p.srv.Serve(ln) }()
	t.Cleanup(func() { _ = p.srv.Close() })

	reg, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, name, "settings-center", "", protocol.RegisterPayload{
		Name: name, Addr: p.URL(), Description: "端到端口户替身",
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

func (p *e2ePortal) URL() string { return "ws://" + p.ln.Addr().String() + "/ws" }

func (p *e2ePortal) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := timerUpgrader.Upgrade(w, r, nil)
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
			resp, _ := protocol.NewResponse(env, "e2e-portal", protocol.ResponsePayload{OK: true})
			writeWS(ws, resp)
			continue
		}
		resp, _ := protocol.NewResponse(env, "e2e-portal", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
		writeWS(ws, resp)
	}
}

// waitTimers 等一条满足条件的快照（快照按 seq 认，不认到达顺序）。
func (p *e2ePortal) waitTimers(t *testing.T, ok func(protocol.TimerStatePayload) bool, timeout time.Duration, what string) protocol.TimerStatePayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		got := append([]protocol.TimerStatePayload(nil), p.got...)
		p.mu.Unlock()
		for _, st := range got {
			if ok(st) {
				return st
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("等门户快照超时（%s）；已收到 %d 条", what, len(p.got))
	return protocol.TimerStatePayload{}
}

func TestTimerSleepWakeE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	timerBin := buildServiceNamed(t, "github.com/NouSart/sump/timer", "sump-timer")

	scPort := freePort(t)
	timerPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	timerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", timerPort)

	// -settings ""：测试中心的覆盖值不落盘——否则本用例设的 max_ms=500 会写进
	// 仓库的 data/settings.json，下次跑（新中心读到旧覆盖）就在前面的用例上翻车。
	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort), "-settings", "")
	waitWSReady(t, centerURL, 60*time.Second)

	// 调用方替身先注册（timer 到点时要能在名册里找到它）；门户替身也注册（验推快照这条链路）。
	caller := startE2ECaller(t, centerURL, "e2e-caller")
	portal := startE2EPortal(t, centerURL, "e2e-portal")

	startProc(t, timerBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", timerPort),
		"-center", centerURL,
		"-portal", "e2e-portal",
	)

	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	waitRosterHas(t, obs, "timer", 60*time.Second)

	cl, err := protocol.Dial(timerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cl.Close() })

	// 断言 a：受理即回执——400ms 的计时，回执不该等它（真进程上同样成立）。
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	rp := callJumpFrom(t, cl, "e2e-caller", "timer", "sleep", protocol.TimerSleepPayload{MS: 400, Note: "e2e 的备注"}, ctx)
	elapsed := time.Since(start)
	if !rp.OK {
		t.Fatalf("sleep 未被受理: %s", rp.Error)
	}
	if elapsed > time.Second {
		t.Fatalf("受理回执花了 %s：受理即回执不该等", elapsed)
	}
	var res protocol.TimerSleepResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.ItemID == "" || res.Phase != "queued" {
		t.Fatalf("受理回执不符: %+v", res)
	}

	// 断言 a2：timer 把**快照**推给了门户（真进程链路；门户据此画倒计时）。
	if st := portal.waitTimers(t, func(s protocol.TimerStatePayload) bool { return len(s.Timers) == 1 }, 10*time.Second, "受理后的计时"); st.Timers[0].ItemID != res.ItemID {
		t.Fatalf("门户快照里的计时项不符: %+v", st.Timers)
	}

	// 断言 b：到点补报（跳回调用方，final）。
	p := caller.waitReport(t, "done", res.ItemID, 15*time.Second)
	if !p.Final {
		t.Fatalf("到点补报应是终态: %+v", p)
	}
	if p.Text == "" {
		t.Fatalf("补报文本为空")
	}
	// 到点 → 门户必须收到一条不含它的快照，倒计时才会消失。
	portal.waitTimers(t, func(s protocol.TimerStatePayload) bool { return len(s.Timers) == 0 }, 10*time.Second, "到点后的空快照")

	// 断言 c：撤销后不再到点，且撤销立刻发一条终态补报（调用方的在途记账靠它释放）。
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	rp2 := callJumpFrom(t, cl, "e2e-caller", "timer", "sleep", protocol.TimerSleepPayload{MS: 1500}, ctx2)
	if !rp2.OK {
		t.Fatalf("sleep 未被受理: %s", rp2.Error)
	}
	var res2 protocol.TimerSleepResult
	if err := protocol.DecodeRaw(rp2.Data, &res2); err != nil {
		t.Fatal(err)
	}
	ctx3, cancel3 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel3()
	rp3 := callJumpFrom(t, cl, "e2e-caller", "timer", "cancel", protocol.TimerCancelPayload{ItemID: res2.ItemID}, ctx3)
	if !rp3.OK {
		t.Fatalf("cancel 被拒: %s", rp3.Error)
	}
	var cr protocol.TimerCancelResult
	if err := protocol.DecodeRaw(rp3.Data, &cr); err != nil {
		t.Fatal(err)
	}
	if cr.Status != "canceled" {
		t.Fatalf("cancel 回执不符: %+v", cr)
	}
	if p := caller.waitReport(t, "canceled", res2.ItemID, 10*time.Second); !p.Final {
		t.Fatalf("撤销补报应是终态: %+v", p)
	}
	caller.absent(t, "done", res2.ItemID, 1800*time.Millisecond)

	// 断言 d：设置热链——经中心把 timer.max_ms 改成 500，新上限立刻生效（900ms 被拒）。
	ctx4, cancel4 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel4()
	rp4 := callJump(t, obs, "settings-center", "set_setting",
		protocol.SetSettingPayload{Service: "timer", Key: "max_ms", Value: "500"}, ctx4)
	if !rp4.OK {
		t.Fatalf("set_setting 被拒: %s", rp4.Error)
	}
	deadline := time.Now().Add(15 * time.Second)
	for {
		ctx5, cancel5 := context.WithTimeout(context.Background(), 10*time.Second)
		rp5 := callJumpFrom(t, cl, "e2e-caller", "timer", "sleep", protocol.TimerSleepPayload{MS: 900}, ctx5)
		cancel5()
		if !rp5.OK {
			if rp5.Error == "" {
				t.Fatalf("应给出拒绝原因")
			}
			break // 新上限已生效
		}
		if time.Now().After(deadline) {
			t.Fatalf("max_ms=500 未在 15 秒内生效（900ms 仍被受理）")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
