// 此包只用于联调；生产服务之间仍只通过协议访问。
package integration_test

import (
	"context"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	approval "github.com/QAQ-awa-QAQ/sump/approval/server"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	reasoner "github.com/QAQ-awa-QAQ/sump/reasoner/server"
	"github.com/QAQ-awa-QAQ/sump/service"
	center "github.com/QAQ-awa-QAQ/sump/settings-center/server"
	shell "github.com/QAQ-awa-QAQ/sump/shell/server"
)

type report struct {
	from    string
	payload protocol.ReportPayload
}
type fixture struct {
	ctx       context.Context
	center    string
	exe, root string
	logger    *log.Logger
	reports   chan report
}

func setup(t *testing.T) *fixture {
	t.Helper()
	exe, err := analysis.FindPowerShell()
	if err != nil {
		t.Skip(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	t.Cleanup(cancel)
	f := &fixture{ctx: ctx, exe: exe, root: t.TempDir(), logger: log.New(io.Discard, "", 0), reports: make(chan report, 100)}
	c, err := center.Start(center.Config{Addr: "127.0.0.1:0", OnReport: func(from string, r protocol.ReportPayload) { f.reports <- report{from, r} }}, f.logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown() })
	f.center = "ws://" + c.Addr() + "/ws"
	return f
}
func (f *fixture) start(t *testing.T, s *service.Service) {
	t.Helper()
	if err := s.Start(f.ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
}
func (f *fixture) approval(t *testing.T, parser analysis.Parser) *approval.Server {
	t.Helper()
	e, err := engine.New(parser, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	a, err := approval.New(approval.Config{Name: "approval", Listen: "127.0.0.1:0", Center: f.center, Engine: e, PolicyJSON: policy.DefaultJSON, Logger: f.logger})
	if err != nil {
		t.Fatal(err)
	}
	f.start(t, a.Service)
	return a
}
func (f *fixture) shell(t *testing.T, approvalName string) {
	t.Helper()
	s := shell.New(shell.Config{Name: "shell", Listen: "127.0.0.1:0", Center: f.center, Root: f.root, Shell: f.exe, ApprovalService: approvalName, ApprovalTimeout: 8 * time.Second}, f.logger)
	f.start(t, s.Service)
}
func (f *fixture) client(t *testing.T, name string) *service.Service {
	t.Helper()
	s := service.New(service.Config{Name: name, Listen: "127.0.0.1:0", Center: f.center, Logger: f.logger})
	return s
}
func (f *fixture) req(command string) protocol.CommandApprovalRequest {
	return protocol.CommandApprovalRequest{Version: 1, ItemID: protocol.NewID(), Plan: protocol.ExecutionPlan{Version: 1, Command: command, WorkDir: f.root, ShellPath: f.exe, Args: protocol.PowerShellArgs(), Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile, TimeoutMS: 5000, MaxOutputBytes: 4096}}
}

func TestTerminalApprovalAndFeedback(t *testing.T) {
	f := setup(t)
	f.approval(t, analysis.PowerShell{Executable: f.exe})
	f.shell(t, "approval")
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
	marker := filepath.Join(f.root, "keep.txt")
	if err := os.WriteFile(marker, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ command, decision, phase string }{
		{"Write-Output 'integration-ok'", "allow", "done"},
		{"Remove-Item '" + strings.ReplaceAll(marker, "'", "''") + "'", "deny", "rejected"},
		{"Get-Date; Get-Location", "review", "rejected"},
	} {
		t.Run(tc.decision, func(t *testing.T) {
			start := time.Now()
			raw, err := c.Call(f.ctx, "shell", "run", protocol.ShellRunPayload{Command: tc.command}, "trace-check", "observer")
			if err != nil {
				t.Fatal(err)
			}
			var ack protocol.ShellRunResult
			if err := protocol.DecodeRaw(raw, &ack); err != nil {
				t.Fatal(err)
			}
			t.Logf("queued ack: %s", time.Since(start))
			phases := []string{}
			for {
				select {
				case r := <-events:
					if r.ItemID != ack.ItemID {
						t.Fatalf("item mismatch: %+v", r)
					}
					var data protocol.ShellEventData
					if err := protocol.DecodeRaw(r.Data, &data); err != nil {
						t.Fatal(err)
					}
					if data.Approval == nil || data.Approval.Decision != tc.decision || data.Approval.PlanDigest == "" {
						t.Fatalf("missing decision feedback: %+v", data)
					}
					phases = append(phases, r.Phase)
					if r.Final {
						if r.Phase != tc.phase {
							t.Fatalf("phase %s", r.Phase)
						}
						if tc.decision == "allow" && (strings.Join(phases, ",") != "approved,done" || !strings.Contains(data.Stdout, "integration-ok")) {
							t.Fatalf("wrong order/output: %v %+v", phases, data)
						}
						if tc.decision != "allow" && data.Stdout != "" {
							t.Fatal("rejected command ran")
						}
						t.Logf("final feedback: %s", time.Since(start))
						return
					}
				case <-f.ctx.Done():
					t.Fatal("feedback timeout")
				}
			}
		})
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("denied deletion happened: %v", err)
	}
	select {
	case r := <-f.reports:
		t.Fatalf("business outcome incorrectly reported as service fault: %+v", r)
	default:
	}
}

type brokenParser struct{ path string }

func (p brokenParser) RuntimePath() string { return p.path }
func (p brokenParser) Parse(context.Context, string) (analysis.Facts, error) {
	return analysis.Facts{}, errors.New("simulated component failure")
}

func TestParserFaultReportedToRealCenter(t *testing.T) {
	f := setup(t)
	f.approval(t, brokenParser{f.exe})
	c := f.client(t, "tester")
	f.start(t, c)
	if _, err := c.Call(f.ctx, "approval", protocol.ApprovalAction, f.req("Get-Date"), "trace", "tester"); err == nil {
		t.Fatal("component failure approved")
	}
	for {
		select {
		case r := <-f.reports:
			if r.from == "approval" && r.payload.Kind == "approval_parser" {
				return
			}
		case <-f.ctx.Done():
			t.Fatal("own fault not reported")
		}
	}
}

func TestMissingApprovalFailsClosedAndEarlyFeedbackRetries(t *testing.T) {
	f := setup(t)
	f.shell(t, "")
	c := f.client(t, "late-observer")
	got := make(chan protocol.AsyncResultPayload, 1)
	calls := 0
	c.Handle("report_result", func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		calls++
		if calls == 1 {
			return map[string]string{"status": "ignored"}, nil
		}
		var r protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(in.Input, &r); err != nil {
			return nil, err
		}
		got <- r
		return map[string]string{"status": "accepted"}, nil
	})
	f.start(t, c)
	if _, err := c.Call(f.ctx, "shell", "run", protocol.ShellRunPayload{Command: "Write-Output should-not-run"}, "trace", "late-observer"); err != nil {
		t.Fatal(err)
	}
	select {
	case r := <-got:
		if r.Phase != "rejected" || !r.Final {
			t.Fatalf("not fail closed: %+v", r)
		}
	case <-f.ctx.Done():
		t.Fatal("early result lost")
	}
}

// recordingLLM emits one tool request, then observes actual reasoner input.
// No API calls or tokens are used by this fixture.
type recordingLLM struct {
	mu       sync.Mutex
	sent     bool
	observed chan string
}

func (f *recordingLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "[审批 allow") || strings.Contains(m.Content, "phase=done") {
			select {
			case f.observed <- m.Content:
			default:
			}
		}
	}
	if !f.sent {
		f.sent = true
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "command-1", Type: "function", Function: llm.FunctionCall{Name: "shell__run", Arguments: `{"command":"Write-Output llm-integration-ok"}`}}}}, nil
	}
	return llm.Message{Role: "assistant", Content: "已收到命令处理反馈。"}, nil
}

func TestRealReasonerReceivesApprovalAndExecutionFeedback(t *testing.T) {
	f := setup(t)
	f.approval(t, analysis.PowerShell{Executable: f.exe})
	f.shell(t, "approval")
	memory := f.client(t, "memory")
	memory.Handle("store", func(context.Context, protocol.Envelope, protocol.JumpPayload) (any, error) {
		return map[string]bool{"stored": true}, nil
	})
	f.start(t, memory)
	delivered := make(chan struct{}, 32)
	boss := f.client(t, "user-service-stub")
	boss.Handle("deliver", func(context.Context, protocol.Envelope, protocol.JumpPayload) (any, error) {
		select {
		case delivered <- struct{}{}:
		default:
		}
		return map[string]bool{"ok": true}, nil
	})
	f.start(t, boss)
	fake := &recordingLLM{observed: make(chan string, 64)}
	r := reasoner.New(reasoner.Config{Name: "reasoner", Listen: "127.0.0.1:0", Center: f.center, LLM: fake}, f.logger)
	f.start(t, r.Service)
	// 注册返回与名册广播异步；等待 memory 实际看到 reasoner，而不是猜测 sleep 时长。
	for {
		if _, _, err := memory.Resolve("reasoner"); err == nil {
			break
		}
		select {
		case <-f.ctx.Done():
			t.Fatal("reasoner not discovered")
		case <-time.After(5 * time.Millisecond):
		}
	}
	_, err := memory.Call(f.ctx, "reasoner", "resume", protocol.ResumePayload{Context: protocol.TaskContext{ConversationID: "approval-fixture", Boss: "user-service-stub", Messages: []protocol.Message{{Role: "user", Content: "运行示例命令"}}}}, "trace-llm", "reasoner")
	if err != nil {
		t.Fatal(err)
	}
	approved, done := false, false
	for !approved || !done {
		select {
		case text := <-fake.observed:
			approved = approved || strings.Contains(text, "[审批 allow")
			done = done || (strings.Contains(text, "phase=done") && strings.Contains(text, "llm-integration-ok"))
		case <-f.ctx.Done():
			t.Fatalf("model feedback missing: approved=%v done=%v", approved, done)
		}
	}
	select {
	case <-delivered:
	case <-f.ctx.Done():
		t.Fatal("user delivery missing")
	}
}
