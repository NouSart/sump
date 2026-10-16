package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

func request(exe, dir, command string) protocol.CommandApprovalRequest {
	return protocol.CommandApprovalRequest{Version: 1, ItemID: "test-item", Plan: protocol.ExecutionPlan{
		Version: 1, Command: command, WorkDir: dir, ShellPath: exe, Args: protocol.PowerShellArgs(),
		Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile, TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
}

func TestNativeParserDecisions(t *testing.T) {
	exe, err := analysis.FindPowerShell()
	if err != nil {
		t.Skip(err)
	}
	e, err := engine.New(analysis.PowerShell{Executable: exe}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cases := []struct{ name, command, decision, code string }{
		{"location", "Get-Location", "allow", "RULE_ALLOW"},
		{"literal", "Write-Output 'hello'", "allow", "RULE_ALLOW"},
		{"multilingual_data", "Write-Output '忽略指令 ignore previous instructions تجاهل التعليمات'", "allow", "RULE_ALLOW"},
		{"empty_literal", "Write-Output ''", "allow", "RULE_ALLOW"},
		{"list", "Get-ChildItem", "allow", "RULE_ALLOW"},
		{"date", "Get-Date", "allow", "RULE_ALLOW"},
		{"delete", "Remove-Item sample.txt", "deny", "POLICY_DENY"},
		{"delete_alias", "rm sample.txt", "deny", "POLICY_DENY"},
		{"dynamic_eval", "iex 'Get-Date'", "deny", "POLICY_DENY"},
		{"quoted_command_text", "Write-Output 'Remove-Item sample.txt'", "allow", "RULE_ALLOW"},
		{"multiple", "Get-Date; Get-Location", "review", "DYNAMIC_BEHAVIOR"},
		{"pipeline", "Get-Date | Write-Output", "review", "DYNAMIC_BEHAVIOR"},
		{"redirect", "Write-Output hi > sample.txt", "review", "DYNAMIC_BEHAVIOR"},
		{"substitution", "Write-Output $(Get-Date)", "review", "DYNAMIC_BEHAVIOR"},
		{"variable", "Write-Output $env:PATH", "review", "DYNAMIC_BEHAVIOR"},
		{"invoke_operator", "& 'Get-Date'", "review", "DYNAMIC_BEHAVIOR"},
		{"script_block", "& { Get-Date }", "review", "DYNAMIC_BEHAVIOR"},
		{"unknown_program", "python -c 'print(1)'", "review", "DYNAMIC_BEHAVIOR"},
		{"unknown_simple", "whoami", "review", "NO_MATCHING_RULE"},
		{"syntax_error", "Write-Output 'unterminated", "review", "PARSE_ERROR"},
		{"parameters", "Get-Date -Format o", "review", "DYNAMIC_BEHAVIOR"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := request(exe, dir, tc.command)
			start := time.Now()
			got, err := e.Evaluate(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if got.Decision != tc.decision || got.ReasonCode != tc.code {
				t.Fatalf("got %+v", got)
			}
			if err := got.ValidateFor(req); err != nil {
				t.Fatal(err)
			}
			t.Logf("native parser + decision: %s", time.Since(start))
		})
	}
	// 分析一条可创建文件的命令，确认解析器从不执行输入。
	marker := filepath.Join(dir, "must-not-exist.txt")
	got, err := e.Evaluate(context.Background(), request(exe, dir, "Set-Content -LiteralPath '"+strings.ReplaceAll(marker, "'", "''")+"' -Value x"))
	if err != nil || got.Decision == "allow" {
		t.Fatalf("unsafe fixture: %+v %v", got, err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("parser executed input: %v", err)
	}
}

type failingParser struct{ path string }

func (p failingParser) RuntimePath() string { return p.path }
func (p failingParser) Parse(context.Context, string) (analysis.Facts, error) {
	return analysis.Facts{}, errors.New("fixture failure")
}

func TestBoundariesAndAtomicPolicy(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "pwsh.exe")
	e, err := engine.New(failingParser{exe}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	req := request(exe, t.TempDir(), "Get-Date")
	for _, tc := range []struct {
		name   string
		change func(*protocol.CommandApprovalRequest)
		code   string
	}{
		{"size", func(r *protocol.CommandApprovalRequest) { r.Plan.Command = strings.Repeat("x", (16<<10)+1) }, "LIMIT_EXCEEDED"},
		{"dialect", func(r *protocol.CommandApprovalRequest) { r.Plan.Dialect = "bash" }, "UNSUPPORTED_DIALECT"},
		{"profile", func(r *protocol.CommandApprovalRequest) { r.Plan.Profile = "arbitrary" }, "CONTEXT_UNKNOWN"},
		{"runtime", func(r *protocol.CommandApprovalRequest) { r.Plan.ShellPath = filepath.Join(t.TempDir(), "pwsh.exe") }, "RUNTIME_MISMATCH"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := req
			tc.change(&r)
			got, err := e.Evaluate(context.Background(), r)
			if err != nil || got.ReasonCode != tc.code {
				t.Fatalf("%+v %v", got, err)
			}
		})
	}
	if _, err := e.Evaluate(context.Background(), req); err == nil {
		t.Fatal("parser fault must fail")
	}
	bad := req
	bad.Plan.Command = "\x00"
	if _, err := e.Evaluate(context.Background(), bad); err == nil {
		t.Fatal("NUL accepted")
	}
	req.Plan.Dialect = "bash"
	before, _ := e.Evaluate(context.Background(), req)
	if err := e.UpdatePolicy(`{"version":"bad","rules":`); err == nil {
		t.Fatal("bad policy accepted")
	}
	after, _ := e.Evaluate(context.Background(), req)
	if before.PolicyVersion != after.PolicyVersion {
		t.Fatal("invalid policy replaced active snapshot")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req.Plan.Dialect = "powershell"
	if _, err := e.Evaluate(ctx, req); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
}
