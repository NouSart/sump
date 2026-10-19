package server

import (
	"context"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestAutomaticApprovalShellPrefersPowerShell(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"bash", "pwsh", "powershell"} {
		if runtime.GOOS == "windows" {
			name += ".exe"
		}
		// 只测试路径选择；这些文件不会执行。
		if err := os.WriteFile(filepath.Join(root, name), []byte("fixture"), 0755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", root)
	s := New(Config{Name: "shell", Root: root, ApprovalService: "approval"}, log.New(io.Discard, "", 0))
	plan, _, err := s.preparePlan(&job{command: "Get-Date", workDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dialect != "powershell" || !strings.EqualFold(strings.TrimSuffix(filepath.Base(plan.ShellPath), ".exe"), "pwsh") {
		t.Fatalf("automatic choice: %+v", plan)
	}
	// 显式设置不被自动探测覆盖；不支持的方言仍由审批拒绝。
	s.shell = "bash"
	plan, _, err = s.preparePlan(&job{command: "pwd", workDir: root})
	if err != nil {
		t.Fatal(err)
	}
	if plan.Dialect != "bash" {
		t.Fatalf("explicit shell replaced: %+v", plan)
	}
}

// TestFeedbackGateDoesNotHoldWindow 验证"补报慢不占窗口"：
// 命令执行完之后窗口立刻该让出来（后面排队的命令能跑），而终态补报仍要等 approved 的投递尝试。
func TestFeedbackGateDoesNotHoldWindow(t *testing.T) {
	s := New(Config{Name: "shell", Root: t.TempDir(), Shell: "nonexistent-shell-fixture"}, log.New(io.Discard, "", 0))
	j := &job{item: "gate-fixture", command: "fixture", workDir: s.rootNow(), progressReady: make(chan struct{})}
	// 造一个"正在跑"的窗口，然后手工走一遍 tryAdvance 的执行路径（等价于调度器做的事）。
	w := &window{id: "w-gate", running: true, cur: j}
	s.winMu.Lock()
	s.windows[w.id] = w
	s.winMu.Unlock()

	done := make(chan struct{})
	go func() {
		phase, text, data := s.executeAndCollect(j)
		s.winMu.Lock()
		w.running = false
		w.cur = nil
		s.winMu.Unlock()
		s.reportAfterProgress(j, phase, text, data) // 卡在 progressReady 上
		close(done)
	}()

	// 执行结束 → 窗口必须已经让出来，而补报还卡着。
	deadline := time.After(3 * time.Second)
	for s.RunningCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("补报把窗口占住了")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatal("终态补报绕过了 approved 的投递顺序")
	default:
	}
	j.finishProgress()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("progressReady 释放后终态补报没有继续")
	}
}

type panicOnApproved struct{}

func (panicOnApproved) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "phase=approved") {
		panic("simulated feedback failure")
	}
	return len(p), nil
}

type fixtureApprover struct{}

func (fixtureApprover) Approve(context.Context, ApprovalRequest) error { return nil }

func TestProgressPanicReleasesGateAndWindow(t *testing.T) {
	s := New(Config{Name: "shell", Root: t.TempDir(), Shell: "nonexistent-shell-fixture", Approver: fixtureApprover{}}, log.New(panicOnApproved{}, "", 0))
	j := &job{item: "panic-fixture", caller: "observer", command: "fixture", workDir: s.rootNow(), progressReady: make(chan struct{})}
	// 先把它登记进窗口，等价于 actionRun 做过的事。
	win, _, err := s.enroll("", j)
	if err != nil {
		t.Fatal(err)
	}
	s.pendingApprovals <- struct{}{}
	s.process(j) // 补报时 panic → recover → failJob
	select {
	case <-j.progressReady:
	default:
		t.Fatal("panic 没有释放 progress 门闸")
	}
	if len(s.pendingApprovals) != 0 {
		t.Fatal("panic 泄漏了审批名额")
	}
	// 窗口不该被这条命令永久卡住：它已出队（审批失败被跳过），窗口应当被回收。
	deadline := time.After(time.Second)
	for s.WindowCount() != 0 {
		select {
		case <-deadline:
			t.Fatalf("panic 把窗口 %s 卡住了", win)
		case <-time.After(time.Millisecond):
		}
	}
}
