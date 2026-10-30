package integration_test

import (
	"context"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/protocol"
	shell "github.com/NouSart/sump/shell/server"
)

// findPOSIXShell 找一个可用的 POSIX shell；没有就跳过。
//
// Windows 上优先 Git Bash 的固定路径：PATH 里的 `bash` 通常是 WSL 启动器，
// 那是另一套文件系统与工具链，跑 Windows 工程目录会处处碰壁。
func findPOSIXShell(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		for _, p := range []string{
			`C:\Program Files\Git\bin\bash.exe`,
			`C:\Program Files (x86)\Git\bin\bash.exe`,
		} {
			if _, err := os.Stat(p); err == nil {
				return p
			}
		}
		t.Skip("没有 Git Bash")
	}
	p, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("没有 bash")
	}
	return p
}

// TestBashReadOnlyAutoApprovedAndPathBoundary 走真实 center → approval → shell 链路：
//
//   - 只读命令命中内置白名单 → 自动放行并真的执行；
//   - 越出工作目录的路径 → 转 review，没有门户就如实拒绝，且命令没跑。
//
// 这条用例同时钉住"审批看到的方言/解释器"与"实际执行用的"是同一个。
func TestBashReadOnlyAutoApprovedAndPathBoundary(t *testing.T) {
	f := setup(t)
	f.approval(t, analysis.PowerShell{Executable: f.exe})
	posix := findPOSIXShell(t)
	sh := shell.New(shell.Config{Name: "shell", Listen: "127.0.0.1:0", Center: f.center, Root: f.root,
		Shell: posix, ApprovalService: "approval", ApprovalTimeout: 8 * time.Second}, f.logger)
	f.start(t, sh.Service)

	events := make(chan protocol.AsyncResultPayload, 16)
	c := f.client(t, "observer")
	c.Handle("report_result", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var r protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(in.Input, &r); err != nil {
			return nil, err
		}
		events <- r
		return map[string]string{"status": "accepted"}, nil
	})
	f.start(t, c)

	for _, tc := range []struct{ name, command, decision, phase string }{
		{"readonly", "pwd", "allow", "done"},
		{"escape_dotdot", "cat ../outside.txt", "review", "rejected"},
		{"write_command", "touch should-not-exist.txt", "review", "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := c.Call(f.ctx, "shell", "run", protocol.ShellRunPayload{Command: tc.command}, "", "observer")
			if err != nil {
				t.Fatal(err)
			}
			var ack protocol.ShellRunResult
			if err := protocol.DecodeRaw(raw, &ack); err != nil {
				t.Fatal(err)
			}
			for {
				select {
				case r := <-events:
					if r.ItemID != ack.ItemID || !r.Final {
						continue
					}
					var data protocol.ShellEventData
					if err := protocol.DecodeRaw(r.Data, &data); err != nil {
						t.Fatal(err)
					}
					if r.Phase != tc.phase {
						t.Fatalf("%s: phase %s, want %s（%s）", tc.command, r.Phase, tc.phase, data.Error)
					}
					if data.Approval == nil || data.Approval.Decision != tc.decision {
						t.Fatalf("%s: approval feedback %+v", tc.command, data.Approval)
					}
					if tc.decision == "allow" && strings.TrimSpace(data.Stdout) == "" {
						t.Fatalf("%s: 放行后应该有输出: %+v", tc.command, data)
					}
					return
				case <-f.ctx.Done():
					t.Fatal("feedback timeout")
				}
			}
		})
	}
	// 被转人工的命令一条都不许落地。
	if _, err := os.Stat(f.root + "/should-not-exist.txt"); err == nil {
		t.Fatal("未获批准的命令写入了文件")
	}
}
