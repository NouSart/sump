package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// approveJob 保留原注入接缝；生产网络裁决使用公共协议，不导入 approval 服务实现。
func (s *Server) approveJob(ctx context.Context, j *job, svc string) error {
	if s.cfg.Approver != nil {
		return s.cfg.Approver.Approve(ctx, ApprovalRequest{ItemID: j.item, Command: j.command, WorkDir: j.workDir, Service: svc})
	}
	if svc == "" {
		return (FailClosed{}).Approve(ctx, ApprovalRequest{})
	}
	plan, env, err := s.preparePlan(j)
	if err != nil {
		return err
	}
	j.plan, j.environment = &plan, env
	req := protocol.CommandApprovalRequest{Version: 1, ItemID: j.item, Plan: plan}
	raw, err := s.Call(ctx, svc, protocol.ApprovalAction, req, j.trace, j.boss)
	if err != nil {
		return fmt.Errorf("审批服务未返回有效结果: %w", err)
	}
	var result protocol.CommandApprovalResult
	if err := protocol.DecodeRaw(raw, &result); err != nil {
		return errors.New("审批响应解码失败")
	}
	if err := result.ValidateFor(req); err != nil {
		return err
	}
	j.approval = &result
	if result.Decision != protocol.DecisionAllow {
		return fmt.Errorf("%s: %s", result.ReasonCode, result.Feedback.Summary)
	}
	// 许可期从**拿到裁决**起算：第二层要等用户点批准，按发起时刻算会必然过期
	//（ValidForMS 上限 30 秒，用户审 5 分钟就白审了）。
	j.approvedUntil = time.Now().Add(time.Duration(result.ValidForMS) * time.Millisecond)
	if !time.Now().Before(j.approvedUntil) {
		return errors.New("审批许可已过期，本次未执行")
	}
	return nil
}

func (s *Server) preparePlan(j *job) (protocol.ExecutionPlan, []string, error) {
	// 与执行侧（commandLine）用同一套探测：审批看到的解释器必须就是最终执行的那个，
	// 否则会出现"审的是 PowerShell、跑的是 bash"这种分歧。
	exe := s.shellNow()
	if exe == "" {
		detected, ok := detectShell(s.logger)
		if !ok {
			return protocol.ExecutionPlan{}, nil, errors.New("没有可用的 shell（可用设置 exec.shell 指定解释器）")
		}
		exe = detected
	}
	exe, err := lookShell(exe)
	if err != nil {
		return protocol.ExecutionPlan{}, nil, err
	}
	exe, err = filepath.Abs(exe)
	if err != nil {
		return protocol.ExecutionPlan{}, nil, err
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(exe), ".exe"))
	// 非 PowerShell 方言没有对应的自动分析器：profile 记 "unsupported"，审批侧据此跳过自动判定、
	// 直接转第二层问用户——不是拒绝，也不是跳过审批。
	dialect, profile := base, "unsupported"
	args := shellArgs(exe, j.command)
	args = args[:len(args)-1]
	env := os.Environ()
	if base == "pwsh" || base == "powershell" {
		dialect, profile = "powershell", protocol.PowerShellProfile
		args = protocol.PowerShellArgs()
		env = protocol.PowerShellEnvironment(env, exe)
	}
	return protocol.ExecutionPlan{Version: 1, Command: j.command, WorkDir: j.workDir, ShellPath: exe, Args: args,
		Dialect: dialect, Platform: runtime.GOOS, Profile: profile, TimeoutMS: s.timeoutNow().Milliseconds(), MaxOutputBytes: s.maxOutputNow()}, env, nil
}

func (j *job) validateBeforeExecution() error {
	if j.plan == nil {
		return nil
	} // 原 Approver 注入接缝保持兼容。
	if j.approval == nil || j.approval.Decision != protocol.DecisionAllow || j.plan.Digest() != j.approval.PlanDigest ||
		j.command != j.plan.Command || j.workDir != j.plan.WorkDir {
		return errors.New("执行内容与获准内容不一致")
	}
	if !time.Now().Before(j.approvedUntil) {
		return errors.New("排队期间审批许可过期，请重新提交命令")
	}
	return nil
}
