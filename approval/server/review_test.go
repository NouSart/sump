// 第二层（用户裁决）的单元测试：挂起 / 批准 / 拒绝 / 超时 / 队列满 / 门户不在线 / 越权裁决。
//
// 这里用假解析器直接把 engine 判成 review（没有命中白名单规则），
// 真进程全链在 approval-integration 里（真 center + 真 shell + 假门户）。
package server

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// reviewParser 把任何命令解析成一条"没有命中规则"的静态命令 → engine 判 review。
type reviewParser struct{ path string }

func (p reviewParser) RuntimePath() string { return p.path }
func (p reviewParser) Parse(context.Context, string) (analysis.Facts, error) {
	return analysis.Facts{Nodes: 1, Commands: []analysis.Command{{Name: "Get-Process", Invocation: "Unknown"}}}, nil
}

func newReviewServer(t *testing.T, over func(*Config)) (*Server, string) {
	t.Helper()
	exe := filepath.Join(t.TempDir(), "pwsh.exe")
	e, err := engine.New(reviewParser{path: exe}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Engine: e, PolicyJSON: policy.DefaultJSON, Logger: log.New(io.Discard, "", 0), Portal: "frontend", ReviewTTL: 2 * time.Second}
	if over != nil {
		over(&cfg)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s.portalReady = func() bool { return true }
	return s, exe
}

func reviewRequest(exe, item string) protocol.JumpPayload {
	req := protocol.CommandApprovalRequest{Version: 1, ItemID: item, Plan: protocol.ExecutionPlan{
		Version: 1, Command: "Get-Process", WorkDir: filepath.Dir(exe), ShellPath: exe, Args: protocol.PowerShellArgs(),
		Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile,
		TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
	raw, err := protocol.EncodePayload(req)
	if err != nil {
		panic(err)
	}
	return protocol.JumpPayload{Action: protocol.ApprovalAction, Input: raw}
}

// waitPending 等一条待审项登记上。
func waitPending(t *testing.T, s *Server, item string) protocol.PendingApproval {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		for _, p := range s.pendingSnapshot() {
			if p.ItemID == item {
				return p
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("待审项 %s 没出现", item)
	return protocol.PendingApproval{}
}

func decidePayload(t *testing.T, item, decision string) protocol.JumpPayload {
	t.Helper()
	raw, err := protocol.EncodePayload(protocol.ApprovalDecision{ItemID: item, Decision: decision, Note: "手工确认"})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.JumpPayload{Action: protocol.ApprovalDecideAction, Input: raw}
}

// TestReviewApproveUnblocksCaller：白名单外 → 挂起 → 用户批准 → 调用方拿到 allow。
func TestReviewApproveUnblocksCaller(t *testing.T) {
	s, exe := newReviewServer(t, nil)
	ctx := context.Background()
	type out struct {
		res protocol.CommandApprovalResult
		err error
	}
	done := make(chan out, 1)
	go func() {
		v, err := s.approve(ctx, protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-approve"))
		if err != nil {
			done <- out{err: err}
			return
		}
		done <- out{res: v.(protocol.CommandApprovalResult)}
	}()

	info := waitPending(t, s, "item-approve")
	if info.Command != "Get-Process" || info.RequestedBy != "shell" || info.ReasonCode != "NO_MATCHING_RULE" {
		t.Fatalf("待审项内容不对: %+v", info)
	}
	if info.ExpiresMS <= info.CreatedMS {
		t.Fatalf("缺 TTL: %+v", info)
	}

	if _, err := s.decide(ctx, protocol.Envelope{From: "frontend"}, decidePayload(t, "item-approve", protocol.DecisionAllow)); err != nil {
		t.Fatalf("裁决被拒: %v", err)
	}
	select {
	case o := <-done:
		if o.err != nil {
			t.Fatalf("调用方拿到错误: %v", o.err)
		}
		if o.res.Decision != protocol.DecisionAllow || o.res.ReasonCode != protocol.ReasonUserApproved {
			t.Fatalf("裁决不对: %+v", o.res)
		}
		if o.res.ValidForMS != 30000 {
			t.Fatalf("许可期应为 30s（用户可以审很久，但批准后执行窗口仍收紧）: %d", o.res.ValidForMS)
		}
		if err := o.res.ValidateFor(mustRequest(t, exe, "item-approve")); err != nil {
			t.Fatalf("裁决过不了执行方的校验: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("批准后调用方没被唤醒")
	}
	if len(s.pendingSnapshot()) != 0 {
		t.Fatal("裁决后待审项没清理")
	}
}

func mustRequest(t *testing.T, exe, item string) protocol.CommandApprovalRequest {
	t.Helper()
	var req protocol.CommandApprovalRequest
	if err := protocol.DecodeRaw(reviewRequest(exe, item).Input, &req); err != nil {
		t.Fatal(err)
	}
	return req
}

// TestReviewDenyUnblocksCaller：用户拒绝 → 调用方拿到 deny，且不带许可期。
func TestReviewDenyUnblocksCaller(t *testing.T) {
	s, exe := newReviewServer(t, nil)
	done := make(chan protocol.CommandApprovalResult, 1)
	go func() {
		v, err := s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-deny"))
		if err == nil {
			done <- v.(protocol.CommandApprovalResult)
		}
	}()
	waitPending(t, s, "item-deny")
	if _, err := s.decide(context.Background(), protocol.Envelope{From: "frontend"}, decidePayload(t, "item-deny", protocol.DecisionDeny)); err != nil {
		t.Fatalf("裁决被拒: %v", err)
	}
	select {
	case res := <-done:
		if res.Decision != protocol.DecisionDeny || res.ReasonCode != protocol.ReasonUserDenied || res.ValidForMS != 0 {
			t.Fatalf("拒绝裁决不对: %+v", res)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("拒绝后调用方没被唤醒")
	}
}

// TestReviewDecideOnlyFromPortal：别的服务不能替用户裁决（裁决权不外借）。
func TestReviewDecideOnlyFromPortal(t *testing.T) {
	s, exe := newReviewServer(t, nil)
	go func() {
		_, _ = s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-guard"))
	}()
	waitPending(t, s, "item-guard")
	if _, err := s.decide(context.Background(), protocol.Envelope{From: "shell"}, decidePayload(t, "item-guard", protocol.DecisionAllow)); err == nil ||
		!strings.Contains(err.Error(), "只有门户") {
		t.Fatalf("非门户裁决应被拒，实际: %v", err)
	}
	if _, err := s.list(context.Background(), protocol.Envelope{From: "shell"}, protocol.JumpPayload{}); err == nil {
		t.Fatal("非门户拉取待审项应被拒")
	}
	if len(s.pendingSnapshot()) != 1 {
		t.Fatal("越权请求不该影响待审项")
	}
}

// TestReviewTimeoutStaysReview：没人裁决 → 到期原样 review（fail-closed），不执行。
func TestReviewTimeoutStaysReview(t *testing.T) {
	s, exe := newReviewServer(t, func(c *Config) { c.ReviewTTL = 80 * time.Millisecond })
	start := time.Now()
	v, err := s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-timeout"))
	if err != nil {
		t.Fatal(err)
	}
	res := v.(protocol.CommandApprovalResult)
	if res.Decision != protocol.DecisionReview || !strings.Contains(res.Feedback.Summary, "超时") {
		t.Fatalf("超时应保持 review 并说明原因: %+v", res)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("超时没有按 TTL 生效")
	}
	if len(s.pendingSnapshot()) != 0 {
		t.Fatal("超时后待审项没清理")
	}
}

// TestReviewQueueFullFailsClosed：挂起队列满 → 直接按 review 返回，不排队。
func TestReviewQueueFullFailsClosed(t *testing.T) {
	s, exe := newReviewServer(t, func(c *Config) { c.MaxPending = 1 })
	go func() {
		_, _ = s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-1"))
	}()
	waitPending(t, s, "item-1")
	v, err := s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-2"))
	if err != nil {
		t.Fatal(err)
	}
	res := v.(protocol.CommandApprovalResult)
	if res.Decision != protocol.DecisionReview || !strings.Contains(res.Feedback.Summary, "队列已满") {
		t.Fatalf("队列满应 fail-closed 并说明: %+v", res)
	}
}

// TestReviewPortalOfflineFailsClosed：门户不在线 → 不挂起（不浪费调用方 10 分钟），立即 review。
func TestReviewPortalOfflineFailsClosed(t *testing.T) {
	s, exe := newReviewServer(t, nil)
	s.portalReady = func() bool { return false }
	start := time.Now()
	v, err := s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-offline"))
	if err != nil {
		t.Fatal(err)
	}
	res := v.(protocol.CommandApprovalResult)
	if res.Decision != protocol.DecisionReview || !strings.Contains(res.Feedback.Summary, "门户不在线") {
		t.Fatalf("门户离线应 fail-closed 并说明: %+v", res)
	}
	if time.Since(start) > time.Second {
		t.Fatal("门户离线不该挂起")
	}
}

// TestReviewDoesNotBlockOtherApprovals：一条挂起时，别的审批照常（白名单命令立刻放行）。
type echoParser struct{ path string }

func (p echoParser) RuntimePath() string { return p.path }
func (p echoParser) Parse(_ context.Context, command string) (analysis.Facts, error) {
	// 按命令原文回一条同名命令：Get-Date 命中白名单 → allow；Get-Process 无规则 → review。
	return analysis.Facts{Nodes: 1, Commands: []analysis.Command{{Name: command, Invocation: "Unknown"}}}, nil
}

func TestReviewDoesNotBlockOtherApprovals(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "pwsh.exe")
	e, err := engine.New(echoParser{path: exe}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{Engine: e, PolicyJSON: policy.DefaultJSON, Logger: log.New(io.Discard, "", 0), Portal: "frontend", ReviewTTL: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	s.portalReady = func() bool { return true }

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = s.approve(context.Background(), protocol.Envelope{From: "shell"}, reviewRequest(exe, "item-hang"))
	}()
	waitPending(t, s, "item-hang")

	// 挂起还在，白名单命令必须立刻拿到 allow。
	start := time.Now()
	v, err := s.approve(context.Background(), protocol.Envelope{From: "shell"}, requestForAllow(t, exe))
	if err != nil {
		t.Fatal(err)
	}
	if v.(protocol.CommandApprovalResult).Decision != protocol.DecisionAllow {
		t.Fatalf("白名单命令应直接放行: %+v", v)
	}
	if time.Since(start) > time.Second {
		t.Fatal("白名单命令被挂起拖慢")
	}
	if _, err := s.decide(context.Background(), protocol.Envelope{From: "frontend"}, decidePayload(t, "item-hang", protocol.DecisionDeny)); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}

func requestForAllow(t *testing.T, exe string) protocol.JumpPayload {
	t.Helper()
	req := protocol.CommandApprovalRequest{Version: 1, ItemID: "item-allow", Plan: protocol.ExecutionPlan{
		Version: 1, Command: "Get-Date", WorkDir: filepath.Dir(exe), ShellPath: exe, Args: protocol.PowerShellArgs(),
		Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile,
		TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
	raw, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return protocol.JumpPayload{Action: protocol.ApprovalAction, Input: raw}
}
