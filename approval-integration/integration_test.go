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

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	approval "github.com/NouSart/sump/approval/server"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	reasoner "github.com/NouSart/sump/reasoner/server"
	"github.com/NouSart/sump/service"
	center "github.com/NouSart/sump/settings-center/server"
	shell "github.com/NouSart/sump/shell/server"
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

// reviewApproval 起一个开着第二层的 approval：门户叫 portal，挂起时长 8 秒（与 shell 的等待对齐）。
func (f *fixture) reviewApproval(t *testing.T, parser analysis.Parser) *approval.Server {
	t.Helper()
	e, err := engine.New(parser, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	a, err := approval.New(approval.Config{Name: "approval", Listen: "127.0.0.1:0", Center: f.center, Engine: e,
		PolicyJSON: policy.DefaultJSON, Logger: f.logger, Portal: "portal", ReviewTTL: 8 * time.Second, MaxPending: 4})
	if err != nil {
		t.Fatal(err)
	}
	f.start(t, a.Service)
	return a
}

// startPortal 起一个假门户：收 approval_pending，按 decide 回调裁决。
func (f *fixture) startPortal(t *testing.T, decide func(item protocol.PendingApproval) (string, string)) *service.Service {
	t.Helper()
	p := service.New(service.Config{Name: "portal", Listen: "127.0.0.1:0", Center: f.center, Logger: f.logger})
	pending := make(chan protocol.PendingApproval, 8)
	p.Handle(protocol.ApprovalPendingAction, func(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
		var item protocol.PendingApproval
		if err := protocol.DecodeRaw(in.Input, &item); err != nil {
			return nil, err
		}
		pending <- item
		return map[string]string{"status": "accepted"}, nil
	})
	f.start(t, p)
	go func() {
		for {
			select {
			case <-f.ctx.Done():
				return
			case item := <-pending:
				decision, note := decide(item)
				if decision == "" {
					continue
				}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				_, err := p.Call(ctx, "approval", protocol.ApprovalDecideAction,
					protocol.ApprovalDecision{ItemID: item.ItemID, Decision: decision, Note: note}, "", "")
				cancel()
				if err != nil {
					t.Logf("门户裁决送达失败: %v", err)
				}
			}
		}
	}()
	return p
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

// TestUserReviewSecondLayer 是第二层的真进程全链：白名单外的命令挂起 → 门户收到待审 → 用户批准 → shell 执行并补报 done。
// 另一个用例验证用户拒绝 → rejected（命令不执行）。
func TestUserReviewSecondLayer(t *testing.T) {
	for _, tc := range []struct {
		name, decision, wantPhase, wantOutcome string
	}{
		{"批准", "allow", "done", "退出码 0"},
		{"拒绝", "deny", "rejected", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := setup(t)
			f.reviewApproval(t, analysis.PowerShell{Executable: f.exe})
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

			seen := make(chan protocol.PendingApproval, 1)
			// 先起门户（approval 只有在名册里看到门户时才挂起）。
			f.startPortal(t, func(item protocol.PendingApproval) (string, string) {
				select {
				case seen <- item:
				default:
				}
				return tc.decision, "集成测试"
			})
			// 等门户与 shell 都进名册（否则 approval 会按"无人裁决"直接 review）。
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				var names []string
				for _, card := range c.Roster() {
					names = append(names, card.Name)
				}
				joined := strings.Join(names, ",")
				if strings.Contains(joined, "portal") && strings.Contains(joined, "shell") && strings.Contains(joined, "approval") {
					break
				}
				time.Sleep(20 * time.Millisecond)
			}

			// Get-Process 不在白名单 → 第一层 review → 第二层挂起等门户。
			raw, err := c.Call(f.ctx, "shell", "run", protocol.ShellRunPayload{Command: "Get-Process"}, "trace-review", "observer")
			if err != nil {
				t.Fatal(err)
			}
			var ack protocol.ShellRunResult
			if err := protocol.DecodeRaw(raw, &ack); err != nil {
				t.Fatal(err)
			}

			var item protocol.PendingApproval
			select {
			case item = <-seen:
			case <-f.ctx.Done():
				t.Fatal("门户没收到待审命令")
			}
			if item.Command != "Get-Process" {
				t.Fatalf("待审命令原文不对: %+v", item)
			}
			if item.ReasonCode != "NO_MATCHING_RULE" {
				t.Fatalf("第一层理由码应是 NO_MATCHING_RULE: %+v", item)
			}
			if item.RequestedBy != "shell" {
				t.Fatalf("待审项应记录发起方 shell: %+v", item)
			}
			if item.ExpiresMS <= item.CreatedMS {
				t.Fatalf("待审项缺 TTL: %+v", item)
			}

			phases := []string{}
			var last protocol.AsyncResultPayload
			for {
				select {
				case r := <-events:
					if r.ItemID != ack.ItemID {
						continue
					}
					phases = append(phases, r.Phase)
					last = r
					if r.Final {
						if r.Phase != tc.wantPhase {
							t.Fatalf("最终阶段应为 %s，实际 %v（%s）", tc.wantPhase, phases, r.Text)
						}
						if tc.wantOutcome != "" && !strings.Contains(r.Text, tc.wantOutcome) {
							t.Fatalf("执行输出没回来: %s", r.Text)
						}
						if tc.decision == "deny" && !strings.Contains(r.Text, "USER_DENIED") && !strings.Contains(r.Text, "拒绝") {
							t.Fatalf("拒绝理由应可见: %s", r.Text)
						}
						return
					}
				case <-f.ctx.Done():
					t.Fatalf("用户裁决没走通完整链: phases=%v last=%+v", phases, last)
				}
			}
		})
	}
}

// TestLLMReviewChain：**全流程**自动化证据（假 LLM，不打真实 API）：
// 模型决定调 shell__run（白名单外命令）→ shell 送审 → approval 挂起 → 门户收到待审并批准
// → shell 执行 → 结果作为工具结果注入回模型 → 最终答复直达 boss。
// 这条覆盖"LLM → shell → 审批 → 用户裁决 → 执行 → 回到 LLM"的完整闭环。
type reviewLLM struct {
	mu       sync.Mutex
	sent     bool
	observed chan string
}

func (f *reviewLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, m := range req.Messages {
		if strings.Contains(m.Content, "USER_APPROVED") || strings.Contains(m.Content, "phase=rejected") || strings.Contains(m.Content, "phase=done") {
			select {
			case f.observed <- m.Content:
			default:
			}
		}
	}
	if !f.sent {
		f.sent = true
		// Get-Process 不在白名单 → 必须走第二层。
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "review-1", Type: "function", Function: llm.FunctionCall{Name: "shell__run", Arguments: `{"command":"Get-Process"}`}}}}, nil
	}
	return llm.Message{Role: "assistant", Content: "命令已由用户批准并执行完毕。"}, nil
}

func TestLLMReviewChain(t *testing.T) {
	f := setup(t)
	f.reviewApproval(t, analysis.PowerShell{Executable: f.exe})
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

	pendingSeen := make(chan protocol.PendingApproval, 1)
	f.startPortal(t, func(item protocol.PendingApproval) (string, string) {
		select {
		case pendingSeen <- item:
		default:
		}
		return protocol.DecisionAllow, "集成测试自动批准"
	})

	fake := &reviewLLM{observed: make(chan string, 64)}
	r := reasoner.New(reasoner.Config{Name: "reasoner", Listen: "127.0.0.1:0", Center: f.center, LLM: fake}, f.logger)
	f.start(t, r.Service)
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
	if _, err := memory.Call(f.ctx, "reasoner", "resume", protocol.ResumePayload{Context: protocol.TaskContext{
		ConversationID: "llm-review", Boss: "user-service-stub",
		Messages: []protocol.Message{{Role: "user", Content: "看看现在有哪些进程"}},
	}}, "trace-llm-review", "reasoner"); err != nil {
		t.Fatal(err)
	}

	select {
	case item := <-pendingSeen:
		if item.Command != "Get-Process" {
			t.Fatalf("挂起的命令不对: %+v", item)
		}
		if item.RequestedBy != "shell" {
			t.Fatalf("发起方应为 shell: %+v", item)
		}
	case <-f.ctx.Done():
		t.Fatal("模型发起的命令没进第二层")
	}

	// 批准回执与执行结果是**两条**注入（approved → done），分别累计。
	approved, done := false, false
	for !approved || !done {
		select {
		case text := <-fake.observed:
			approved = approved || strings.Contains(text, "USER_APPROVED")
			done = done || strings.Contains(text, "phase=done")
			if strings.Contains(text, "phase=rejected") {
				t.Fatalf("模型侧看到拒绝: %s", text)
			}
		case <-f.ctx.Done():
			t.Fatal("执行结果没回到模型")
		}
	}
	select {
	case <-delivered:
	case <-f.ctx.Done():
		t.Fatal("最终答复没送达 boss")
	}
}
