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

func TestFeedbackGateDoesNotHoldExecutionSlot(t *testing.T) {
	s := New(Config{Name: "shell", Root: t.TempDir(), Shell: "nonexistent-shell-fixture"}, log.New(io.Discard, "", 0))
	j := &job{command: "fixture", workDir: s.rootNow(), progressReady: make(chan struct{})}
	s.running = 1
	done := make(chan struct{})
	go func() { s.execute(j); close(done) }()
	deadline := time.After(3 * time.Second)
	for s.RunningCount() != 0 {
		select {
		case <-deadline:
			j.finishProgress()
			t.Fatal("feedback gate held execution slot")
		case <-time.After(time.Millisecond):
		}
	}
	select {
	case <-done:
		t.Fatal("terminal feedback bypassed progress ordering")
	default:
	}
	j.finishProgress()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("feedback gate not released")
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

func TestProgressPanicReleasesGateAndSlot(t *testing.T) {
	s := New(Config{Name: "shell", Root: t.TempDir(), Shell: "nonexistent-shell-fixture", Approver: fixtureApprover{}}, log.New(panicOnApproved{}, "", 0))
	j := &job{item: "panic-fixture", caller: "observer", command: "fixture", workDir: s.rootNow(), progressReady: make(chan struct{})}
	s.pendingApprovals <- struct{}{}
	s.process(j)
	select {
	case <-j.progressReady:
	default:
		t.Fatal("panic did not release progress gate")
	}
	deadline := time.After(time.Second)
	for s.RunningCount() != 0 {
		select {
		case <-deadline:
			t.Fatal("panic leaked execution slot")
		case <-time.After(time.Millisecond):
		}
	}
	if len(s.pendingApprovals) != 0 {
		t.Fatal("panic leaked approval slot")
	}
}
