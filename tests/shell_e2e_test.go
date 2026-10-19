// 跨进程总测试：settings-center + shell（真进程）。
//
// 场景：中心起 → shell 注册（名片声明 run 为“受理即回执”async）→ 调用方替身发起 run：
//   - 受理回执立刻返回（不等审批、不等执行）：{item_id, state: queued}；
//   - 未接入审批（默认 FailClosed）→ 补报 report_result（phase=rejected, final=true）回到调用方；
//   - 工作目录越界 → 同步拒绝（请求本身不合法，不入队列）。
//
// 覆盖的是跨进程的三条边界（受理 / 拒绝 / 补报）。命令的“执行路径”由 shell 自带测试覆盖
// （注入审批实现后跑真子进程）；fail-closed 下 true 进程不会执行任何命令。
package tests

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// reportProbe 是调用方替身：注册进中心，收 shell 的补报（report_result）。
// 名字必须是 "e2e-observer"——补报发往“发起跳转时的 From”，而 callJump 的 From 就是它。
type reportProbe struct {
	ln      net.Listener
	srv     *http.Server
	reports chan protocol.AsyncResultPayload
}

func startReportProbe(t *testing.T, centerURL string) *reportProbe {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &reportProbe{ln: ln, reports: make(chan protocol.AsyncResultPayload, 16)}
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
	env, err := protocol.NewEnvelope(protocol.TypeRegister, "e2e-observer", "settings-center", "", protocol.RegisterPayload{
		Name: "e2e-observer", Addr: p.URL(), Description: "shell e2e 调用方替身",
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
	return p
}

func (p *reportProbe) URL() string { return "ws://" + p.ln.Addr().String() + "/ws" }

func (p *reportProbe) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := probeUpgrader.Upgrade(w, r, nil)
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
			resp, _ := protocol.NewResponse(env, "e2e-observer", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
			writeWS(ws, resp)
			continue
		}
		var pl protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(jp.Input, &pl); err == nil {
			select {
			case p.reports <- pl:
			default:
			}
		}
		resp, _ := protocol.NewResponse(env, "e2e-observer", protocol.ResponsePayload{OK: true})
		writeWS(ws, resp)
	}
}

// waitReport 等一条指定阶段的补报。
func (p *reportProbe) waitReport(t *testing.T, phase string, timeout time.Duration) protocol.AsyncResultPayload {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case r := <-p.reports:
			if r.Phase == phase {
				return r
			}
		case <-deadline:
			t.Fatalf("等待 phase=%s 的补报超时", phase)
		}
	}
}

// TestShellHotPathE2E：跨进程走通“受理即回执 + 补报”。
func TestShellHotPathE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	shBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/shell")

	scPort := freePort(t)
	shPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	shellURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", shPort)
	root := t.TempDir()

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)
	startProc(t, shBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", shPort),
		"-center", centerURL,
		"-root", root,
	)

	probe := startReportProbe(t, centerURL)
	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	waitRosterHas(t, obs, "shell", 60*time.Second)

	// 断言 a：名片跨进程带来了 async 声明（受理即回执的动作契约）。
	ctxA, cancelA := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelA()
	rpRoster := callJump(t, obs, "settings-center", "roster", nil, ctxA)
	if !rpRoster.OK {
		t.Fatalf("取名册失败: %s", rpRoster.Error)
	}
	var roster protocol.RosterPayload
	if err := protocol.DecodeRaw(rpRoster.Data, &roster); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, card := range roster.Services {
		if card.Name != "shell" {
			continue
		}
		for _, p := range card.Provides {
			if p.Action == "run" {
				found = true
				if !p.Async || !p.Tool {
					t.Fatalf("run 应声明 async + tool: %+v", p)
				}
			}
		}
	}
	if !found {
		t.Fatal("名册里 shell 的名片没有 run")
	}

	// 断言 b：受理即回执——回执立刻返回，且只表示“已入队列”。
	caller, err := protocol.Dial(shellURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = caller.Close() })
	ctxB, cancelB := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancelB()
	start := time.Now()
	rp := callJump(t, caller, "shell", "run", map[string]any{"command": "echo hi"}, ctxB)
	if !rp.OK {
		t.Fatalf("run 未被受理: %s", rp.Error)
	}
	var res protocol.ShellRunResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.ItemID == "" || res.Phase != "queued" {
		t.Fatalf("受理回执不符: %+v", res)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("受理回执拖了 %v（受理不该等审批 / 执行）", elapsed)
	}

	// 断言 c：未接入审批 → 补报未通过（终态），句柄对得上。
	p := probe.waitReport(t, "rejected", 30*time.Second)
	if p.ItemID != res.ItemID {
		t.Fatalf("补报句柄不符: %s != %s", p.ItemID, res.ItemID)
	}
	if !p.Final {
		t.Fatal("rejected 应为终态")
	}
	if !strings.Contains(p.Text, "未执行") {
		t.Fatalf("补报文本未说明未执行: %q", p.Text)
	}
	var data protocol.ShellEventData
	if err := protocol.DecodeRaw(p.Data, &data); err != nil {
		t.Fatal(err)
	}
	if data.Error == "" {
		t.Fatal("结构化补充应带上拒绝原因")
	}

	// 断言 d：工作目录越界 → 同步拒绝（不入队列、无补报）。
	ctxD, cancelD := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelD()
	if rp := callJump(t, caller, "shell", "run", map[string]any{
		"command":  "echo hi",
		"work_dir": "..",
	}, ctxD); rp.OK {
		t.Fatal("越界的工作目录应被同步拒绝")
	}
}
