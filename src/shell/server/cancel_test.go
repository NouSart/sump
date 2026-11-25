package server

// 取消执行面的包内测试：真实跑一条短命令，覆盖「终止失败 → unknown」这条
// 端到端测试里难以稳定触发的分支（killTree 可注入），以及账本里取消句柄的生命周期。

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

// sleepShell 给出一个"睡 ms 毫秒"的解释器与命令（测试显式钉住解释器，不靠自动探测的运气）。
func sleepShell(t *testing.T, ms int) (shell, command string) {
	t.Helper()
	if p, err := exec.LookPath("pwsh"); err == nil {
		return p, fmt.Sprintf("Start-Sleep -Milliseconds %d", ms)
	}
	if p, err := exec.LookPath("powershell"); err == nil {
		return p, fmt.Sprintf("Start-Sleep -Milliseconds %d", ms)
	}
	if p, err := exec.LookPath("bash"); err == nil {
		return p, fmt.Sprintf("sleep %.3f", float64(ms)/1000)
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p, fmt.Sprintf("sleep %.3f", float64(ms)/1000)
	}
	t.Skip("找不到可用的 shell，跳过需要真子进程的用例")
	return "", ""
}

// waitTrackedExec 轮询账本到指定执行维。
func waitTrackedExec(t *testing.T, s *Server, item, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		last, _ = s.trackedExec(item, time.Now())
		if last == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s 内 %s 没到 exec=%s（当前 %q）", within, item, want, last)
}

// TestOwnerControlCancelUnconfirmedWhenKillFails：终止调用失败 → 控制面必须回
// unknown + outcome_unknown，账本也记 unknown——不许把"没杀掉"写成 cancelled / done。
//
// 注入的 kill 只报错、不动进程；命令（睡 800ms）随后自然结束，控制面在收尾后
// 按账本回答。这样不用等满 WaitDelay（10s），也不用制造一个假死的子进程。
func TestOwnerControlCancelUnconfirmedWhenKillFails(t *testing.T) {
	sh, sleepCmd := sleepShell(t, 800)
	old := killProcessTree
	killProcessTree = func(*os.Process) error { return errors.New("simulated taskkill denial") }
	t.Cleanup(func() { killProcessTree = old })

	s := New(Config{Name: "shell", Root: t.TempDir(), Shell: sh, Approver: fixtureApprover{}},
		log.New(io.Discard, "", 0))
	body, err := protocol.EncodePayload(protocol.ShellRunPayload{Command: sleepCmd})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.run("", "w-cancel-unconfirmed", body)
	if err != nil {
		t.Fatal(err)
	}
	res := out.(protocol.ShellRunResult)
	waitTrackedExec(t, s, res.ItemID, protocol.OwnerExecRunning, 5*time.Second)

	gotExec, reason, err := s.OwnerControl(context.Background(), res.ItemID, protocol.OwnerOpCancel, 0)
	if err != nil {
		t.Fatalf("取消失败不应变成 RPC 错误（应回 unknown + reason）: %v", err)
	}
	if gotExec != protocol.OwnerExecUnknown || reason != cancelReasonUnknown {
		t.Fatalf("终止失败应回 unknown/outcome_unknown，实际 exec=%q reason=%q", gotExec, reason)
	}
	if got, ok := s.trackedExec(res.ItemID, time.Now()); !ok || got != protocol.OwnerExecUnknown {
		t.Fatalf("账本应记 unknown（不假成功），实际 %q ok=%v", got, ok)
	}
	s.trackMu.Lock()
	rec, ok := s.tracked[res.ItemID]
	s.trackMu.Unlock()
	if !ok || rec.ctl != nil {
		t.Fatalf("终态记录不该继续持有取消句柄: ok=%v ctl=%v", ok, rec.ctl)
	}
}

// TestTrackClearsCancelHandleOnTerminal：取消句柄挂在账本上，终态即摘——句柄数量因此
// 与账本同一口径，不会随终态保留窗口滞留。
func TestTrackClearsCancelHandleOnTerminal(t *testing.T) {
	s := newTrackedServer(t)
	ctl := newJobControl()
	s.trackWith("a", protocol.OwnerExecRunning, ctl)
	if rec, ok := s.trackedRecord("a", time.Now()); !ok || rec.ctl != ctl {
		t.Fatalf("在跑记录应持有取消句柄: ok=%v ctl=%v", ok, rec.ctl)
	}
	s.track("a", protocol.OwnerExecDone)
	rec, ok := s.trackedRecord("a", time.Now())
	if !ok {
		t.Fatal("终态在保留窗口内应仍被认识")
	}
	if rec.ctl != nil {
		t.Fatal("终态记录应摘掉取消句柄（避免控制面拿到已收尾的取消器 / 泄漏）")
	}
}
