package tests

// owner 侧控制面（R-029）的 shell 行为：owner_status 认识/不认识受理句柄；
// cancel / pause / resume 没有路径 → not_supported（不假装成功）。

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
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

// TestOwnerStatusAndUnsupportedControl：在跑的命令可被 status 看到；三种控制都回
// not_supported 并指明对象；不认识的句柄回 not_found；跑完后 status 仍是 done。
func TestOwnerStatusAndUnsupportedControl(t *testing.T) {
	sh, sleepCmd := slowPowerShell(t, 3)
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

	// shell 没有暂停/恢复/终止路径：三种 op 都回 not_supported（不是"已取消"）。
	for _, op := range []string{protocol.OwnerOpPause, protocol.OwnerOpResume, protocol.OwnerOpCancel} {
		_, err := ownerRequest(t, c, "owner_control", protocol.OwnerControlPayload{ID: res.ItemID, Op: op})
		if !spv2.IsCode(err, spv2.CodeNotSupported) {
			t.Fatalf("%s 没有实现路径，应回 not_supported，实际 %v", op, err)
		}
		if !strings.Contains(err.Error(), res.ItemID) {
			t.Fatalf("%s 的错误信息应指明对象 %q: %v", op, res.ItemID, err)
		}
	}

	// 不认识的句柄：not_found（先认对象，再说能力）。
	if _, err := ownerRequest(t, c, "owner_control", protocol.OwnerControlPayload{
		ID: "never-accepted", Op: protocol.OwnerOpCancel,
	}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("不认识的句柄应回 not_found，实际 %v", err)
	}

	// 跑完：终态在保留窗口内，status 给 done（不能变成"不认识"）。
	waitReportItem(t, cl, "done", res.ItemID)
	if got := ownerStatus(t, c, res.ItemID).States[res.ItemID]; got != protocol.OwnerExecDone {
		t.Fatalf("跑完后执行维应为 done，实际 %q", got)
	}
}

// TestOwnerStatusFailedJob：审批未通过（未执行）的受理记录是 failed，不写成 done。
func TestOwnerStatusFailedJob(t *testing.T) {
	sc := startStubCenter(t)
	cl := startCaller(t, sc, "caller")
	// Approver 未注入 + approval.service 为空 = FailClosed。
	s := startShell(t, sc, server.Config{Root: t.TempDir()})
	c := dial(t, s.WsURL())

	res := run(t, c, echoCommand("x"), "")
	waitReportItem(t, cl, "rejected", res.ItemID)

	st := ownerStatus(t, c, res.ItemID)
	if st.States[res.ItemID] != protocol.OwnerExecFailed {
		t.Fatalf("被拒执行的受理记录应为 failed: %+v", st.States)
	}
}
