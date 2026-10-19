// Package engine 将执行描述转换为终端裁决与 LLM 反馈。它不执行待审命令。
package engine

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync/atomic"
	"unicode/utf8"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

type Engine struct {
	parser analysis.Parser
	policy atomic.Pointer[policy.Snapshot]
	// tier 是自动批准档位（off / low / medium），默认 off = 只走 policy 白名单。
	tier atomic.Value
}

func New(parser analysis.Parser, policyJSON string) (*Engine, error) {
	if parser == nil {
		return nil, errors.New("缺少命令解析器")
	}
	e := &Engine{parser: parser}
	e.tier.Store(TierOff)
	if err := e.UpdatePolicy(policyJSON); err != nil {
		return nil, err
	}
	return e, nil
}

func (e *Engine) UpdatePolicy(text string) error {
	snapshot, err := policy.Load(text)
	if err != nil {
		return err
	}
	e.policy.Store(snapshot)
	return nil
}

func (e *Engine) Evaluate(ctx context.Context, req protocol.CommandApprovalRequest) (protocol.CommandApprovalResult, error) {
	p := req.Plan
	if req.Version != protocol.ApprovalVersion || p.Version != 1 || req.ItemID == "" || len(req.ItemID) > 128 {
		return protocol.CommandApprovalResult{}, errors.New("无效审批版本或 item_id")
	}
	if strings.TrimSpace(p.Command) == "" || !utf8.ValidString(p.Command) || strings.ContainsRune(p.Command, 0) {
		return protocol.CommandApprovalResult{}, errors.New("command 必须是非空、不含 NUL 的 UTF-8 文本")
	}
	snapshot := e.policy.Load() // 一次处理从始至终只使用同一份规则。
	result := protocol.CommandApprovalResult{
		Version: protocol.ApprovalVersion, ItemID: req.ItemID, DecisionID: protocol.NewID(),
		Decision: protocol.DecisionReview, PlanDigest: p.Digest(), PolicyVersion: snapshot.Version,
	}
	finish := func(decision, code, summary string) (protocol.CommandApprovalResult, error) {
		result.Decision, result.ReasonCode, result.Feedback.Summary = decision, code, summary
		if decision == protocol.DecisionAllow {
			result.ValidForMS = 30000
		}
		return result, nil
	}
	if len(p.Command) > analysis.MaxCommandBytes {
		return finish(protocol.DecisionReview, "LIMIT_EXCEEDED", "命令超过 16 KiB，本次未执行")
	}
	if p.Dialect != "powershell" && p.Dialect != "bash" || p.Platform != runtime.GOOS {
		return finish(protocol.DecisionReview, "UNSUPPORTED_DIALECT", "只支持本机 PowerShell 与 POSIX shell 方言；本次未执行")
	}
	if p.Dialect == "bash" {
		return e.evaluateBash(ctx, p, snapshot, finish)
	}
	base := strings.ToLower(strings.TrimSuffix(filepath.Base(p.ShellPath), ".exe"))
	if p.Profile != protocol.PowerShellProfile || !filepath.IsAbs(p.WorkDir) || !filepath.IsAbs(p.ShellPath) ||
		(base != "powershell" && base != "pwsh") || !slices.Equal(p.Args, protocol.PowerShellArgs()) || p.TimeoutMS <= 0 || p.MaxOutputBytes <= 0 {
		return finish(protocol.DecisionReview, "CONTEXT_UNKNOWN", "执行目录、解释器或固定启动环境不完整，本次未执行")
	}
	// 请求方不能指定解析器要启动的程序；只接受本服务已选定的可信解释器。
	sameRuntime := p.ShellPath == e.parser.RuntimePath()
	if runtime.GOOS == "windows" {
		sameRuntime = strings.EqualFold(p.ShellPath, e.parser.RuntimePath())
	}
	if !sameRuntime {
		return finish(protocol.DecisionReview, "RUNTIME_MISMATCH", "审批与执行的 PowerShell 路径不一致，请统一配置后重新提交")
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	facts, err := e.parser.Parse(ctx, p.Command)
	if err != nil {
		return result, fmt.Errorf("解析组件不可用: %w", err)
	}
	if len(facts.SyntaxErrors) > 0 {
		result.Feedback.Unknowns = facts.SyntaxErrors
		return finish(protocol.DecisionReview, "PARSE_ERROR", "命令语法不完整或无效，本次未执行")
	}
	for _, cmd := range facts.Commands {
		if snapshot.Denied(cmd.Name) {
			return finish(protocol.DecisionDeny, "POLICY_DENY", "命令包含明确禁止的操作："+cmd.Name+"；本次未执行")
		}
	}
	if facts.Nodes > 4096 || len(facts.Unknowns) > 0 || len(facts.Commands) != 1 {
		result.Feedback.Unknowns = facts.Unknowns
		if len(result.Feedback.Unknowns) == 0 {
			result.Feedback.Unknowns = []string{"unsupported_command_structure"}
		}
		return finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR", "命令包含组合、动态或当前未覆盖的结构；本次未执行，需要进一步审批")
	}
	cmd := facts.Commands[0]
	if cmd.Invocation != "Unknown" { // & 和 . 都是额外的调用语义，首版不自动允许。
		return finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR", "显式调用或点载入不在首版允许范围内")
	}
	rule, ok := snapshot.Rule(cmd.Name)
	if !ok || len(cmd.Arguments) > rule.MaxArgs {
		return finish(protocol.DecisionReview, "NO_MATCHING_RULE", "没有覆盖该命令及参数范围的允许规则，本次未执行")
	}
	for _, arg := range cmd.Arguments {
		if arg.Kind != "StringConstantExpressionAst" && arg.Kind != "ConstantExpressionAst" {
			return finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR", "参数包含非静态内容，本次未执行")
		}
	}
	result.Feedback.RuleIDs = []string{rule.ID}
	return finish(protocol.DecisionAllow, "RULE_ALLOW", rule.Reason)
}
