// Package tests 是 shell 的服务内集成测试：最小“设置中心”替身 + 真 shell 实例，
// 走真实 WS 通信，命令用真实子进程执行（执行路径本身不假）。
package tests

import (
	"context"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/shell/server"
)

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// ---------- 设置中心的最小替身 ----------

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

// ---------- 调用方替身：收补报 ----------

type caller struct {
	ln      net.Listener
	srv     *http.Server
	reports chan protocol.AsyncResultPayload
	seen    []protocol.AsyncResultPayload // 已到但当前断言还用不上的补报（不能丢，见 waitReportItem）
}

func startCaller(t *testing.T, sc *stubCenter, name string) *caller {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &caller{ln: ln, reports: make(chan protocol.AsyncResultPayload, 16)}
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
			case c.reports <- p:
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

// waitReport 取一条指定阶段的补报（超时即失败）。
// 只有一条命令在跑时用它；并存多条命令时必须用 waitReportItem 认句柄。
func waitReport(t *testing.T, c *caller, phase string) protocol.AsyncResultPayload {
	t.Helper()
	return takeReport(t, c, phase, "")
}

// waitReportItem 取指定条目（句柄）的指定阶段补报。
//
// 为什么按句柄等：补报由骨架的 Fire 各自成 goroutine 投递，**条目之间的到达顺序没有保证**
// （CI 上实测到过：第三条的「被拒」早于第二条的「已排队」）。所以这里
//   - 不匹配的补报留着而不丢掉——丢掉会把后面还要用的那条吃掉，最后一条等待必然超时；
//   - 断言认句柄，不认「第几条到」。
func waitReportItem(t *testing.T, c *caller, phase, itemID string) protocol.AsyncResultPayload {
	t.Helper()
	return takeReport(t, c, phase, itemID)
}

func takeReport(t *testing.T, c *caller, phase, itemID string) protocol.AsyncResultPayload {
	t.Helper()
	deadline := time.After(15 * time.Second)
	for {
		for i, p := range c.seen {
			if p.Phase == phase && (itemID == "" || p.ItemID == itemID) {
				c.seen = append(c.seen[:i], c.seen[i+1:]...)
				return p
			}
		}
		select {
		case p := <-c.reports:
			c.seen = append(c.seen, p)
		case <-deadline:
			t.Fatalf("等待补报超时：phase=%s item=%q；已收到 %v", phase, itemID, phasesOf(c.seen))
		}
	}
}

// phasesOf 列出已到的补报，超时时一眼看出是谁先到了。
func phasesOf(ps []protocol.AsyncResultPayload) []string {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		out = append(out, fmt.Sprintf("%s/%s", p.Phase, p.ItemID))
	}
	return out
}

// TestWaitReportItemIgnoresArrivalOrder：补报按**倒序**到达（CI 上真实发生过），
// 三条仍应各自认领得到——旧写法（不匹配即丢）在这里会丢掉 a 的"被拒"，然后卡在等待。
func TestWaitReportItemIgnoresArrivalOrder(t *testing.T) {
	c := &caller{reports: make(chan protocol.AsyncResultPayload, 8)}
	c.reports <- protocol.AsyncResultPayload{ItemID: "c", Phase: "rejected", Text: "队列已满"}
	c.reports <- protocol.AsyncResultPayload{ItemID: "b", Phase: "approved", Text: "已排队"}
	c.reports <- protocol.AsyncResultPayload{ItemID: "a", Phase: "approved", Text: "开始执行"}

	for _, tc := range []struct{ item, phase, text string }{
		{"a", "approved", "开始执行"},
		{"b", "approved", "已排队"},
		{"c", "rejected", "队列已满"},
	} {
		if p := waitReportItem(t, c, tc.phase, tc.item); p.Text != tc.text {
			t.Fatalf("%s 的补报认错: %+v", tc.item, p)
		}
	}
}

// ---------- shell 实例 ----------

// allowAll 是测试用的“一律通过”裁决（生产默认是 FailClosed，见 approval.go）。
type allowAll struct{}

func (allowAll) Approve(context.Context, server.ApprovalRequest) error { return nil }

func startShell(t *testing.T, sc *stubCenter, cfg server.Config) *server.Server {
	t.Helper()
	cfg.Name = "shell"
	cfg.Listen = "127.0.0.1:0"
	cfg.Center = sc.URL()
	cfg.HeartbeatInterval = 100 * time.Millisecond
	logger := log.New(os.Stdout, "[shell-test] ", log.LstdFlags|log.Lmicroseconds)
	s := server.New(cfg, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

// jump 对 shell 发起一次跳转并返回响应 payload。
func jump(t *testing.T, c *protocol.Client, to, action string, input any) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "caller", to, "", protocol.JumpPayload{Action: action, Input: raw})
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

// run 发起一次 run 并断言受理回执的形状（受理即回执：state=queued + 句柄非空）。
func run(t *testing.T, c *protocol.Client, command, workdir string) protocol.ShellRunResult {
	t.Helper()
	rp := jump(t, c, "shell", "run", protocol.ShellRunPayload{Command: command, WorkDir: workdir})
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
	return res
}

// eventData 解出补报里的结构化补充。
func eventData(t *testing.T, p protocol.AsyncResultPayload) protocol.ShellEventData {
	t.Helper()
	var d protocol.ShellEventData
	if err := protocol.DecodeRaw(p.Data, &d); err != nil {
		t.Fatalf("补报 data 解析失败: %v", err)
	}
	return d
}

// slowCommand 给出“睡 n 秒”的可移植命令，并返回要用哪个解释器执行它
// （测试显式钉住解释器，不靠自动探测的运气；找不到任何一个就跳过用例）。
func slowCommand(t *testing.T, seconds int) (shell string, command string) {
	t.Helper()
	n := strconv.Itoa(seconds)
	if p, err := exec.LookPath("bash"); err == nil {
		return p, "sleep " + n
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p, "sleep " + n
	}
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p, "Start-Sleep -Seconds " + n
	}
	if p, err := exec.LookPath("powershell"); err == nil {
		return p, "Start-Sleep -Seconds " + n
	}
	if p, err := exec.LookPath("cmd"); err == nil {
		return p, "timeout /t " + n + " /nobreak >nul"
	}
	t.Skip("找不到可用的 shell，跳过与时长相关的用例")
	return "", ""
}

// echoCommand 给出“输出一段文本”的可移植命令（bash / cmd / pwsh 都有 echo）。
func echoCommand(text string) string { return "echo " + text }

// ---------- 用例 ----------

// TestRunIsAcceptedThenRejectedByApproval：受理即回执（不等执行），未接入审批 → 命令不执行、补报未通过。
func TestRunIsAcceptedThenRejectedByApproval(t *testing.T) {
	root := t.TempDir()
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: root}) // 不注入 Approver → FailClosed
	c := dial(t, s.WsURL())

	marker := filepath.Join(root, "marker.txt")
	res := run(t, c, "echo sump > "+marker, "")

	p := waitReport(t, cl, "rejected")
	if p.ItemID != res.ItemID {
		t.Fatalf("补报句柄不符: %s != %s", p.ItemID, res.ItemID)
	}
	if !p.Final {
		t.Fatal("rejected 应为终态（final=true）")
	}
	if !strings.Contains(p.Text, "审批未通过") {
		t.Fatalf("补报文本未说明未通过: %q", p.Text)
	}
	if d := eventData(t, p); d.Error == "" {
		t.Fatal("结构化补充应带上拒绝原因")
	}
	// 未通过 = 真的没执行。
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("未通过审批的命令却执行了")
	}
}

// TestRunExecutesAfterApproval：审批通过 → approved（非终态）→ done（终态）带退出码与输出。
func TestRunExecutesAfterApproval(t *testing.T) {
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}})
	c := dial(t, s.WsURL())

	res := run(t, c, echoCommand("sump-shell-ok"), "")
	approved := waitReport(t, cl, "approved")
	if approved.Final {
		t.Fatal("approved 不是终态（其后还有执行结果）")
	}
	done := waitReport(t, cl, "done")
	if !done.Final || done.ItemID != res.ItemID {
		t.Fatalf("done 补报不符: %+v", done)
	}
	d := eventData(t, done)
	if d.ExitCode != 0 {
		t.Fatalf("退出码不符: %+v", d)
	}
	if !strings.Contains(d.Stdout, "sump-shell-ok") {
		t.Fatalf("stdout 未包含输出: %q", d.Stdout)
	}
	if d.Shell == "" {
		t.Fatal("结构化补充应记录实际使用的 shell")
	}
}

// TestWorkDirBoundary：工作目录越界 / 不存在 / 指向文件 → 同步拒绝（请求本身不合法，不入队列）。
func TestWorkDirBoundary(t *testing.T) {
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := os.Mkdir(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "not-a-dir.txt")
	if err := os.WriteFile(file, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: root, Approver: allowAll{}})
	c := dial(t, s.WsURL())

	if rp := jump(t, c, "shell", "run", protocol.ShellRunPayload{Command: echoCommand("x"), WorkDir: "sub"}); !rp.OK {
		t.Fatalf("根目录内的子目录应可用: %s", rp.Error)
	}
	waitReport(t, cl, "done")

	for _, wd := range []string{"..", filepath.Join(root, ".."), filepath.Join(root, "nope"), file} {
		rp := jump(t, c, "shell", "run", protocol.ShellRunPayload{Command: echoCommand("x"), WorkDir: wd})
		if rp.OK {
			t.Fatalf("越界 / 不存在 / 非目录的工作目录应被拒: %q", wd)
		}
	}
}

// TestQueueLimitRejectsWhenFull：并行上限 1 + 队列上限 1 → 第三条被立即拒绝（不排队等待）。
func TestQueueLimitRejectsWhenFull(t *testing.T) {
	sh, sleep := slowCommand(t, 2)
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Shell: sh, Approver: allowAll{}, Concurrency: 1, QueueLimit: 1})
	c := dial(t, s.WsURL())

	first := run(t, c, sleep, "")
	second := run(t, c, sleep, "")
	third := run(t, c, sleep, "")

	// 三条都有补报，按句柄认领（到达顺序不保证，见 waitReportItem）。
	if p := waitReportItem(t, cl, "approved", first.ItemID); !strings.Contains(p.Text, "准备执行") {
		t.Fatalf("第一条应已取得执行槽位: %q", p.Text)
	}
	if p := waitReportItem(t, cl, "approved", second.ItemID); !strings.Contains(p.Text, "已排队") {
		t.Fatalf("第二条应已排队: %q", p.Text)
	}
	if p := waitReportItem(t, cl, "rejected", third.ItemID); !strings.Contains(p.Text, "队列已满") {
		t.Fatalf("拒绝原因应为队列已满: %q", p.Text)
	}
	// 前两条最终都跑完（排队的那条会被补位）。
	waitReportItem(t, cl, "done", first.ItemID)
	waitReportItem(t, cl, "done", second.ItemID)
}

// TestTimeoutTerminates：执行超时 → 记录 timed_out 与退出码 -1；命令被终止（不是任它跑完）。
func TestTimeoutTerminates(t *testing.T) {
	sh, sleep := slowCommand(t, 5)
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Shell: sh, Approver: allowAll{}, Timeout: 1200 * time.Millisecond})
	c := dial(t, s.WsURL())

	start := time.Now()
	run(t, c, sleep, "")
	p := waitReport(t, cl, "done")
	d := eventData(t, p)
	if !d.TimedOut || d.ExitCode != -1 {
		t.Fatalf("超时应记录 timed_out 与退出码 -1: %+v", d)
	}
	if elapsed := time.Since(start); elapsed > 4*time.Second {
		t.Fatalf("超时后应立即终止（耗时 %v）", elapsed)
	}
	if !strings.Contains(p.Text, "命令超时") {
		t.Fatalf("补报文本应说明超时: %q", p.Text)
	}
}

// TestOutputTruncated：单流输出超过上限 → 截断并标注总数（命令照常跑完）。
func TestOutputTruncated(t *testing.T) {
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}, MaxOutputBytes: 1024})
	c := dial(t, s.WsURL())

	run(t, c, echoCommand(strings.Repeat("x", 4000)), "")
	waitReport(t, cl, "approved")
	p := waitReport(t, cl, "done")
	d := eventData(t, p)
	if !d.Truncated {
		t.Fatalf("应标记截断: %+v", d)
	}
	if d.StdoutBytes <= 1024 {
		t.Fatalf("stdout_bytes 应为真实总数: %d", d.StdoutBytes)
	}
	if len(d.Stdout) > 1024 {
		t.Fatalf("保留的 stdout 不应超过上限: %d", len(d.Stdout))
	}
	if !strings.Contains(p.Text, "已截断") {
		t.Fatalf("补报文本应标注截断: %q", p.Text)
	}
}

// TestCommandRequired：空命令 → 同步拒绝。
func TestCommandRequired(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}})
	c := dial(t, s.WsURL())

	if rp := jump(t, c, "shell", "run", protocol.ShellRunPayload{Command: "   "}); rp.OK {
		t.Fatal("空命令应被拒")
	}
}

// TestChineseOutputDecoded：Windows PowerShell 5.1 在中文系统上按系统代码页（GBK）写 stdout，
// 直接当 UTF-8 解会把汉字全换成替换符（模型看到的就是 `2026�10�17�`）。
// 这里用**纯 ASCII 命令**产生中文输出（[char]0x4E2D+[char]0x6587 = 中文），避免参数编码干扰。
// 非 Windows / 无 PowerShell 的环境跳过（该缺陷只在 Windows 代码页下出现）。
func TestChineseOutputDecoded(t *testing.T) {
	if filepath.Separator != '\\' {
		t.Skip("只有 Windows 的 PowerShell 5.1 会按系统代码页输出")
	}
	pwsh, err := exec.LookPath("powershell")
	if err != nil {
		if pwsh, err = exec.LookPath("pwsh"); err != nil {
			t.Skip("没有 PowerShell")
		}
	}
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Shell: pwsh, Approver: allowAll{}})
	c := dial(t, s.WsURL())

	run(t, c, "Write-Output ([char]0x4E2D + [char]0x6587)", "")
	waitReport(t, cl, "approved")
	done := waitReport(t, cl, "done")
	d := eventData(t, done)
	if d.ExitCode != 0 {
		t.Fatalf("退出码不符: %+v", d)
	}
	if !strings.Contains(d.Stdout, "中文") {
		t.Fatalf("中文输出被解坏了（应含「中文」）: %q", d.Stdout)
	}
	if strings.ContainsRune(d.Stdout, 0xFFFD) {
		t.Fatalf("输出里出现了替换符（说明按错误编码解）: %q", d.Stdout)
	}
}
