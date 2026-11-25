package tests

// owner 侧控制面（R-029）的 shell 行为：owner_status 认识/不认识受理句柄；
// cancel 有真实执行面（在跑的命令会被真的终止）；pause / resume 仍没有路径。

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/shell/server"
	"github.com/NouSart/sump/spv2"
)

// slowPowerShell 给出"睡 n 秒"的 PowerShell 解释器与命令。
//
// 不用 slowCommand：它优先选 bash，而 Windows 上的 C:\windows\system32\bash.exe 是 WSL
// 启动器（本机未装发行版时会一直挂着），会把测试挂死。这里显式钉住 PowerShell。
func slowPowerShell(t *testing.T, seconds int) (shell, command string) {
	t.Helper()
	cmd := fmt.Sprintf("Start-Sleep -Seconds %d", seconds)
	for _, name := range []string{"pwsh", "powershell"} {
		if p, err := exec.LookPath(name); err == nil {
			return p, cmd
		}
	}
	t.Skip("找不到 PowerShell，跳过需要长命令的用例")
	return "", ""
}

// ownerRequest 以 "memory" 的名义发一次原生 call（owner 控制面的调用方站位）。
func ownerRequest(t *testing.T, c *spv2.Client, method string, in any) (msgpack.RawMessage, error) {
	t.Helper()
	f, err := spv2.NewCall("w-owner", spv2.ServiceRef("memory"), spv2.ServiceRef("shell"), method, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return msgpack.RawMessage(resp.Body), nil
}

// ownerStatus 发一次 owner_status 并解出结果。
func ownerStatus(t *testing.T, c *spv2.Client, ids ...string) protocol.OwnerStatusResult {
	t.Helper()
	raw, err := ownerRequest(t, c, "owner_status", protocol.OwnerStatusPayload{IDs: ids})
	if err != nil {
		t.Fatalf("owner_status 失败: %v", err)
	}
	var st protocol.OwnerStatusResult
	if err := protocol.DecodeRaw(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// ownerControl 发一次 owner_control 并解出确认后的执行维。
func ownerControl(t *testing.T, c *spv2.Client, id, op string) protocol.OwnerControlResult {
	t.Helper()
	raw, err := ownerRequest(t, c, "owner_control", protocol.OwnerControlPayload{ID: id, Op: op})
	if err != nil {
		t.Fatalf("owner_control %s %s 失败: %v", op, id, err)
	}
	var res protocol.OwnerControlResult
	if err := protocol.DecodeRaw(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// waitOwnerExec 轮询到指定执行维（受理后先 created、审批通过再 running，存在短暂窗口）。
func waitOwnerExec(t *testing.T, c *spv2.Client, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		last = ownerStatus(t, c, id).States[id]
		if last == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s 内 %s 没到 exec=%s（当前 %q）", within, id, want, last)
}

// TestOwnerStatusAndCancelRunningCommand：在跑的命令可被 status 看到；pause / resume 仍回
// not_supported；cancel 必须**真的终止**它（远早于命令自然结束），账本与补报都记 cancelled。
func TestOwnerStatusAndCancelRunningCommand(t *testing.T) {
	sh, sleepCmd := slowPowerShell(t, 5)
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Shell: sh, Approver: allowAll{}})
	c := dial(t, s.WsURL())

	res := run(t, c, sleepCmd, "")
	waitOwnerExec(t, c, res.ItemID, protocol.OwnerExecRunning, 5*time.Second)

	// status：认识的句柄给执行维，不认识的 id 不出现在 states 里；recovery_complete 显式 true。
	st := ownerStatus(t, c, res.ItemID, "never-accepted")
	if !st.RecoveryComplete {
		t.Fatalf("recovery_complete 应为 true（shell 不持久化受理记录）: %+v", st)
	}
	if st.States[res.ItemID] != protocol.OwnerExecRunning {
		t.Fatalf("在跑的命令执行维应为 running: %+v", st.States)
	}
	if _, ok := st.States["never-accepted"]; ok {
		t.Fatalf("不认识的 id 不该出现在 states 里: %+v", st.States)
	}

	// pause / resume 仍没有执行面：not_supported，且错误信息指明对象。
	for _, op := range []string{protocol.OwnerOpPause, protocol.OwnerOpResume} {
		_, err := ownerRequest(t, c, "owner_control", protocol.OwnerControlPayload{ID: res.ItemID, Op: op})
		if !spv2.IsCode(err, spv2.CodeNotSupported) {
			t.Fatalf("%s 没有实现路径，应回 not_supported，实际 %v", op, err)
		}
		if !strings.Contains(err.Error(), res.ItemID) {
			t.Fatalf("%s 的错误信息应指明对象 %q: %v", op, res.ItemID, err)
		}
	}

	// cancel 在跑的命令：命令睡 5 秒，取消必须在远早于自然结束前确认终止。
	start := time.Now()
	ctl := ownerControl(t, c, res.ItemID, protocol.OwnerOpCancel)
	if ctl.Exec != protocol.OwnerExecCancelled || ctl.Reason != "" {
		t.Fatalf("取消成功应回 exec=cancelled（reason 空），实际 %+v", ctl)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消应真的终止命令，而不是等它自然结束（耗时 %v，命令要睡 5s）", elapsed)
	}

	// 执行路径的补报：cancelled 终态、退出码 -1、文案不说"执行完成"。
	p := waitReportItem(t, cl, "cancelled", res.ItemID)
	if !p.Final {
		t.Fatal("cancelled 应为终态（final=true）")
	}
	d := eventData(t, p)
	if d.ExitCode != -1 {
		t.Fatalf("被取消的命令退出码应为 -1: %+v", d)
	}
	if !strings.Contains(p.Text, "已被取消") {
		t.Fatalf("补报文本应说明命令已被取消: %q", p.Text)
	}
	if strings.Contains(p.Text, "执行完成") {
		t.Fatalf("被取消的命令不得说成执行完成: %q", p.Text)
	}

	// 账本与补报一致：终态是 cancelled；执行名额已归还（执行路径确实收尾了）。
	if got := ownerStatus(t, c, res.ItemID).States[res.ItemID]; got != protocol.OwnerExecCancelled {
		t.Fatalf("取消后执行维应为 cancelled，实际 %q", got)
	}
	if n := s.RunningCount(); n != 0 {
		t.Fatalf("取消后执行名额应已归还，仍有 %d 条在跑", n)
	}
}

// TestOwnerControlCancelFinishedJob：已终态的句柄没有可取消的执行——如实回它的终态 + reason，
// 不谎称取消成功。
func TestOwnerControlCancelFinishedJob(t *testing.T) {
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}})
	c := dial(t, s.WsURL())

	res := run(t, c, echoCommand("sump-shell-done"), "")
	waitReportItem(t, cl, "done", res.ItemID)

	ctl := ownerControl(t, c, res.ItemID, protocol.OwnerOpCancel)
	if ctl.Exec != protocol.OwnerExecDone {
		t.Fatalf("已结束的命令应如实回 done，实际 %+v", ctl)
	}
	if ctl.Reason != "already_terminal" {
		t.Fatalf("应说明没有可取消的执行（already_terminal），实际 reason=%q", ctl.Reason)
	}
	if got := ownerStatus(t, c, res.ItemID).States[res.ItemID]; got != protocol.OwnerExecDone {
		t.Fatalf("cancel 不该改写已终态的执行维，实际 %q", got)
	}
}

// TestOwnerControlCancelUnknownID：不认识的句柄回 not_found（先认对象，再说能力）。
func TestOwnerControlCancelUnknownID(t *testing.T) {
	sc := startStubCenter(t)
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: allowAll{}})
	c := dial(t, s.WsURL())

	if _, err := ownerRequest(t, c, "owner_control", protocol.OwnerControlPayload{
		ID: "never-accepted", Op: protocol.OwnerOpCancel,
	}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("不认识的句柄应回 not_found，实际 %v", err)
	}
}

// gatedApprover 先声明已进入审批，再一直等到 ctx 结束——用来钉住「取消能中断等裁决」。
type gatedApprover struct {
	once    sync.Once
	entered chan struct{}
}

func (g *gatedApprover) Approve(ctx context.Context, _ server.ApprovalRequest) error {
	g.once.Do(func() { close(g.entered) })
	<-ctx.Done()
	return ctx.Err()
}

// TestOwnerControlCancelDuringApproval：命令还没开跑（审批中）时取消——不启动命令、
// 不用等满审批超时，账本与补报都是 cancelled（这是能确认的「已停止」）。
func TestOwnerControlCancelDuringApproval(t *testing.T) {
	approver := &gatedApprover{entered: make(chan struct{})}
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	s := startShell(t, sc, server.Config{Root: t.TempDir(), Approver: approver, ApprovalTimeout: 30 * time.Second})
	c := dial(t, s.WsURL())

	res := run(t, c, echoCommand("must-not-run"), "")
	select {
	case <-approver.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("审批没有被调用")
	}

	start := time.Now()
	ctl := ownerControl(t, c, res.ItemID, protocol.OwnerOpCancel)
	if ctl.Exec != protocol.OwnerExecCancelled {
		t.Fatalf("审批中的取消应回 cancelled，实际 %+v", ctl)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("取消应中断等裁决，而不是等满 approval.timeout（耗时 %v）", elapsed)
	}

	p := waitReportItem(t, cl, "cancelled", res.ItemID)
	if !strings.Contains(p.Text, "未执行") {
		t.Fatalf("审批阶段取消应说明命令未执行: %q", p.Text)
	}
	if got := ownerStatus(t, c, res.ItemID).States[res.ItemID]; got != protocol.OwnerExecCancelled {
		t.Fatalf("取消后执行维应为 cancelled，实际 %q", got)
	}
}
