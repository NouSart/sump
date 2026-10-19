package engine_test

import (
	"context"
	"runtime"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// bashRequest 构造一条 POSIX shell 的执行描述（解释器用 Git Bash 的典型路径名）。
func bashRequest(command string) protocol.CommandApprovalRequest {
	shell := "/bin/bash"
	if runtime.GOOS == "windows" {
		shell = `C:\Program Files\Git\bin\bash.exe`
	}
	return protocol.CommandApprovalRequest{Version: 1, ItemID: "bash-item", Plan: protocol.ExecutionPlan{
		Version: 1, Command: command, WorkDir: `C:\work`, ShellPath: shell, Args: []string{"-c"},
		Dialect: "bash", Platform: runtime.GOOS, Profile: "unsupported", TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
}

// TestBashReadOnlyAllowlist：只读命令直过，其余一律转人工。
//
// 这条用例的红/绿直接对应"哪些命令能不经过用户"——改白名单必须同步改它。
func TestBashReadOnlyAllowlist(t *testing.T) {
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct{ name, command, decision, code string }{
		{"pwd", "pwd", "allow", "RULE_ALLOW"},
		{"list", "ls", "allow", "RULE_ALLOW"},
		{"list_flags", "ls -la", "allow", "RULE_ALLOW"},
		{"read_file", "cat main.go", "allow", "RULE_ALLOW"},
		{"quoted_arg", "echo 'hello world'", "allow", "RULE_ALLOW"},
		{"grep", "grep -n func main.go", "allow", "RULE_ALLOW"},
		{"git_status", "git status", "allow", "RULE_ALLOW"},
		{"git_log", "git log --oneline", "allow", "RULE_ALLOW"},

		// 写操作：不在白名单 → 转人工（不是拒绝，用户仍可批准）。
		// rm 例外：它在默认 deny_commands 里，直接硬拒。
		{"remove", "rm -rf build", "deny", "POLICY_DENY"},
		{"move", "mv a b", "review", "NO_MATCHING_RULE"},
		{"write_file", "touch x", "review", "NO_MATCHING_RULE"},
		{"git_push", "git push origin main", "review", "NO_MATCHING_RULE"},
		{"git_commit", "git commit -m msg", "review", "NO_MATCHING_RULE"},

		// 动态结构：一律转人工。
		{"pipeline", "ls | grep x", "review", "DYNAMIC_BEHAVIOR"},
		{"redirect", "cat a > b", "review", "DYNAMIC_BEHAVIOR"},
		{"subshell", "cat $(whoami)", "review", "DYNAMIC_BEHAVIOR"},
		{"variable", "cat $HOME/.bashrc", "review", "DYNAMIC_BEHAVIOR"},
		{"double_quoted_var", `echo "$HOME"`, "review", "DYNAMIC_BEHAVIOR"},
		{"glob", "ls *.go", "review", "DYNAMIC_BEHAVIOR"},
		{"home_tilde", "ls ~/x", "review", "DYNAMIC_BEHAVIOR"},
		{"two_commands", "pwd; ls", "review", "DYNAMIC_BEHAVIOR"},
		{"background", "sleep 1 &", "review", "DYNAMIC_BEHAVIOR"},
		{"unterminated", "echo 'oops", "review", "DYNAMIC_BEHAVIOR"},

		// 明确不收录的命令：会写文件、会执行、会泄露环境变量。
		{"env_leak", "env", "review", "NO_MATCHING_RULE"},
		{"find_delete", "find . -delete", "review", "NO_MATCHING_RULE"},
		{"sed_inplace", "sed -i s/a/b/ f", "review", "NO_MATCHING_RULE"},
		{"interpreter", "python script.py", "review", "NO_MATCHING_RULE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := e.Evaluate(context.Background(), bashRequest(tc.command))
			if err != nil {
				t.Fatalf("%s: %v", tc.command, err)
			}
			if got.Decision != tc.decision || got.ReasonCode != tc.code {
				t.Fatalf("%s: got %s/%s, want %s/%s（%s）", tc.command, got.Decision, got.ReasonCode, tc.decision, tc.code, got.Feedback.Summary)
			}
			if got.Decision == protocol.DecisionAllow && got.ValidForMS <= 0 {
				t.Fatalf("%s: 放行必须带许可期", tc.command)
			}
		})
	}
}

// TestBashRejectsNonShellPath：方言写 bash 但解释器不是 POSIX shell → 转人工。
//
// 防的是"用 PowerShell 路径 + bash 方言"绕过 PowerShell 侧的固定参数校验。
func TestBashRejectsNonShellPath(t *testing.T) {
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	req := bashRequest("pwd")
	req.Plan.ShellPath = `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`
	got, err := e.Evaluate(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != protocol.DecisionReview || got.ReasonCode != "CONTEXT_UNKNOWN" {
		t.Fatalf("got %s/%s", got.Decision, got.ReasonCode)
	}
}

// TestBashHonoursDenyList：配置里的 deny 优先于只读白名单。
func TestBashHonoursDenyList(t *testing.T) {
	e, err := engine.New(analysis.PowerShell{Executable: "unused"},
		`{"version":"t","deny_commands":["ls"],"rules":[]}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := e.Evaluate(context.Background(), bashRequest("ls"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Decision != protocol.DecisionDeny || got.ReasonCode != "POLICY_DENY" {
		t.Fatalf("got %s/%s", got.Decision, got.ReasonCode)
	}
}

// TestBashPathBoundary：只读命令也受路径边界约束。
//
// 白名单管"能做什么"（只读），这条管"能碰哪里"（工作目录内、非秘密文件）。
func TestBashPathBoundary(t *testing.T) {
	e, err := engine.New(analysis.PowerShell{Executable: "unused"}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	work := t.TempDir()
	cases := []struct{ name, command, decision, code string }{
		{"inside_relative", "cat sub/file.txt", "allow", "RULE_ALLOW"},
		{"inside_dot", "cat ./README.md", "allow", "RULE_ALLOW"},
		{"bare_name", "cat README.md", "allow", "RULE_ALLOW"},
		{"option_with_path", "grep -n func main.go", "allow", "RULE_ALLOW"},

		{"escape_dotdot", "cat ../outside.txt", "review", "PATH_OUTSIDE"},
		{"escape_nested", "cat a/../../outside.txt", "review", "PATH_OUTSIDE"},
		{"absolute_posix", "cat /etc/passwd", "review", "PATH_OUTSIDE"},
		{"absolute_windows", "cat C:/Windows/win.ini", "review", "PATH_OUTSIDE"},
		{"secret_file", "cat data/memory-secret.json", "review", "PATH_OUTSIDE"},
		{"secret_via_option", "grep --file=data/x-secret.json pattern", "review", "PATH_OUTSIDE"},
		{"secret_bare", "cat memory-secret.json", "review", "PATH_OUTSIDE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := bashRequest(tc.command)
			req.Plan.WorkDir = work
			got, err := e.Evaluate(context.Background(), req)
			if err != nil {
				t.Fatalf("%s: %v", tc.command, err)
			}
			if got.Decision != tc.decision || got.ReasonCode != tc.code {
				t.Fatalf("%s: got %s/%s, want %s/%s（%s）", tc.command, got.Decision, got.ReasonCode, tc.decision, tc.code, got.Feedback.Summary)
			}
		})
	}
}
