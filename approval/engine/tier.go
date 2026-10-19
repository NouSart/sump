package engine

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/QAQ-awa-QAQ/sump/approval/analysis"
	"github.com/QAQ-awa-QAQ/sump/approval/policy"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// Tier 是自动批准档位：决定"哪些命令不用人点"的松紧。
//
// 三档都只放宽**自动放行**的范围，不放宽拒绝名单（deny_commands 在任何档位下优先），
// 也不改变"判不出来就转人工"这条底线——档位越松，能自动放行的越多，人的点击越少。
type Tier string

const (
	// TierOff 只放行 policy 白名单里的单条简单命令（旧行为）。
	TierOff Tier = "off"
	// TierLow 只读类直过：单条只读命令，以及由若干只读段/级拼起来的组合命令。
	TierLow Tier = "low"
	// TierMedium 黑名单模式：不碰明确危险动作就放行（含 python -c 这类判不出意图的）。
	TierMedium Tier = "medium"
	// TierWorkspace 工作区模式：工作区内的读/写/删/移/构建/跑脚本/本地 git 全部放行，
	// 只拦越过工作区的五类动作（越界路径、提权装系统软件、联网外发、管道进解释器、碰密钥）。
	TierWorkspace Tier = "workspace"
)

// MaxCompoundStages 是一条命令里允许自动判定的"管道级"总数上限：
// 超过就当结构过大转人工（与单条命令 16 KiB 的上限同一思路）。
const MaxCompoundStages = 64

// ParseTier 解析档位；不认识的值一律报错（宁可拒绝也不默默当成某档）。
func ParseTier(v string) (Tier, error) {
	switch Tier(strings.ToLower(strings.TrimSpace(v))) {
	case TierOff:
		return TierOff, nil
	case TierLow:
		return TierLow, nil
	case TierMedium:
		return TierMedium, nil
	case TierWorkspace:
		return TierWorkspace, nil
	}
	return "", fmt.Errorf("自动批准档位无效：%q（可选 off / low / medium / workspace）", v)
}

// TierLabel 是给人看的档位名（卡片与说明里用）。
func TierLabel(t Tier) string {
	switch t {
	case TierLow:
		return "低"
	case TierMedium:
		return "中"
	case TierWorkspace:
		return "工作区"
	}
	return "关"
}

// UpdateTier 切换到指定档位；解析失败时保持旧档位。
func (e *Engine) UpdateTier(v string) error {
	t, err := ParseTier(v)
	if err != nil {
		return err
	}
	e.tier.Store(t)
	return nil
}

// Tier 返回当前生效的档位。
func (e *Engine) Tier() Tier {
	if t, ok := e.tier.Load().(Tier); ok {
		return t
	}
	return TierOff
}

// stageVerdict 是一段里某一级（一条简单命令）的判定结论。
type stageVerdict struct {
	ok     bool
	code   string // 不通过时的理由码（与第一层的码保持同一套词汇）
	text   string // 人话：为什么不过
	ruleID string
	// cwdAfter 只被 cd 改写：后续段按新的工作目录校验路径。
	cwdAfter string
}

// evaluateBashTier 按档位判定一条 POSIX shell 命令（低/中档；"关"档走 evaluateBash 老路径）。
//
// 逐段逐级判：任何一段不达标就整条转人工，并在 summary 里点名是哪一段、为什么；
// 全部达标才整体放行。
func (e *Engine) evaluateBashTier(ctx context.Context, p protocol.ExecutionPlan, snapshot *policy.Snapshot, tier Tier, finish bashFinisher) (protocol.CommandApprovalResult, error) {
	if err := ctx.Err(); err != nil {
		return protocol.CommandApprovalResult{}, err
	}
	compound, err := (analysis.Bash{}).ParseCompound(ctx, p.Command)
	if err != nil {
		return protocol.CommandApprovalResult{}, err
	}
	if len(compound.Unknowns) > 0 {
		result, err := finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR",
			"命令结构当前不自动判定（"+strings.Join(compound.Unknowns, ", ")+"）；本次未执行，需要进一步审批")
		if err == nil {
			result.Feedback.Unknowns = compound.Unknowns
		}
		return result, err
	}
	if totalStages(compound) > MaxCompoundStages {
		return finish(protocol.DecisionReview, "LIMIT_EXCEEDED", "命令由太多段组成，超出自动判定范围；本次未执行，需要进一步审批")
	}
	// 拒绝名单优先于任何档位：只要命令里出现被禁的名字，整条硬拒（不是转人工）。
	for _, name := range compoundNames(compound) {
		if snapshot.Denied(name) {
			return finish(protocol.DecisionDeny, "POLICY_DENY", "命令包含明确禁止的操作："+name+"；本次未执行")
		}
	}
	// root 是沙箱边界（不随 cd 变），cwd 是当前解析基准（cd 之后会变）。
	root, cwd := p.WorkDir, p.WorkDir
	var ruleIDs []string
	for segIdx, seg := range compound.Segments {
		segNo := segIdx + 1
		if len(seg.Stages) == 0 {
			return finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR",
				fmt.Sprintf("第 %d 段里没有可执行的命令；本次未执行，需要进一步审批", segNo))
		}
		if blocksPipeIntoInterpreter(tier) {
			// 管道进解释器要看到"上一级的输出接过来"，按整段判。
			if bad, why := checkPipeIntoInterpreter(seg); bad {
				result, err := finish(protocol.DecisionReview, "DYNAMIC_BEHAVIOR",
					fmt.Sprintf("第 %d 段没过：「%s」档不放行——%s；本次未执行，需要进一步审批", segNo, TierLabel(tier), why))
				return result, err
			}
		}
		for stageIdx, stage := range seg.Stages {
			var v stageVerdict
			switch tier {
			case TierMedium:
				v = checkMediumStage(stage, cwd, root)
			case TierWorkspace:
				v = checkWorkspaceStage(stage, cwd, root)
			default:
				v = checkLowStage(stage, cwd, root)
			}
			if v.cwdAfter != "" {
				cwd = v.cwdAfter
			}
			if !v.ok {
				result, err := finish(protocol.DecisionReview, v.code, formatStageFailure(segNo, stageIdx, stage, v.text, tier))
				if err == nil {
					result.Feedback.Unknowns = dedupe(stage.Unknowns)
				}
				return result, err
			}
			if v.ruleID != "" {
				ruleIDs = append(ruleIDs, v.ruleID)
			}
		}
	}
	summary := fmt.Sprintf("整条命令 %d 段都是只读，按「%s」档自动放行", len(compound.Segments), TierLabel(tier))
	switch tier {
	case TierMedium:
		summary = "命令不涉及明确危险动作，按「中」档自动放行"
	case TierWorkspace:
		summary = "命令都在工作区内、不涉及明确越界动作，按「工作区」档自动放行"
	}
	if len(compound.Segments) == 1 && len(compound.Segments[0].Stages) == 1 {
		// 单条简单命令：沿用规则自带的说明，话说得更具体。
		st := compound.Segments[0].Stages[0]
		if rule, ok := bashRuleFor(st.Command.Name, st.Command.Arguments); ok {
			summary = rule.Reason + "（自动批准档位：" + TierLabel(tier) + "）"
		}
	}
	result, err := finish(protocol.DecisionAllow, "TIER_ALLOW", summary)
	if err != nil {
		return result, err
	}
	result.Feedback.RuleIDs = dedupe(ruleIDs)
	return result, nil
}

// formatStageFailure 把失败点写成卡片上读得懂的一句：第几段、第几级、命令是什么、为什么。
func formatStageFailure(segNo, stageIdx int, stage analysis.Stage, why string, tier Tier) string {
	where := fmt.Sprintf("第 %d 段", segNo)
	if stageIdx > 0 {
		where += fmt.Sprintf("的第 %d 级", stageIdx+1)
	}
	cmd := stage.Command.Name
	if cmd == "" {
		cmd = "（无命令）"
	}
	return fmt.Sprintf("%s「%s」没过：「%s」档不放行——%s；本次未执行，需要进一步审批",
		where, cmd, TierLabel(tier), why)
}

// checkLowStage 判"低"档的一段一级：只读、参数都是字面量、路径都在工作目录内。
//
// 这一档只多认一种结构：分段与管道。每一级都必须单独是只读命令——只要有一段要写、
// 要装、要跑解释器，整条就转人工。
func checkLowStage(stage analysis.Stage, cwd, root string) stageVerdict {
	if len(stage.Unknowns) > 0 {
		return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "含展开或转义（" + strings.Join(dedupe(stage.Unknowns), ", ") + "），值不确定"}
	}
	for _, r := range stage.Redirects {
		if r.Op != "<" {
			return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "重定向会写文件"}
		}
		if bad, reason := bashPathViolationWithin([]analysis.Argument{{Kind: analysis.LiteralArgument, Value: r.Target}}, cwd, root); bad != "" {
			return stageVerdict{code: "PATH_OUTSIDE", text: reason}
		}
	}
	args := stage.Command.Arguments
	for _, a := range args {
		if a.Kind != analysis.LiteralArgument {
			return stageVerdict{code: "DYNAMIC_BEHAVIOR", text: "参数含动态内容（" + a.Value + "），值不确定"}
		}
	}
	// cd 不改文件也不改系统，只挪动这条命令自己进程的工作目录：
	// 只要跳不出工作目录就可以放行，并把后续段的相对路径按新目录校验。
	if strings.EqualFold(stage.Command.Name, "cd") {
		if len(args) != 1 {
			return stageVerdict{code: "NO_MATCHING_RULE", text: "cd 不带目录会切到家目录，超出工作目录"}
		}
		target := args[0].Value
		if bad, reason := bashPathViolationWithin([]analysis.Argument{{Kind: analysis.LiteralArgument, Value: target}}, cwd, root); bad != "" {
			return stageVerdict{code: "PATH_OUTSIDE", text: reason}
		}
		next, ok := resolveBashPath(target, cwd)
		if !ok {
			return stageVerdict{code: "PATH_OUTSIDE", text: "参数是工作目录之外的绝对路径，本次未执行"}
		}
		rel, err := filepath.Rel(root, next)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return stageVerdict{code: "PATH_OUTSIDE", text: "目标目录越出工作目录"}
		}
		return stageVerdict{ok: true, ruleID: "bash-cd", cwdAfter: next}
	}
	rule, ok := bashRuleFor(stage.Command.Name, args)
	if !ok {
		return stageVerdict{code: "NO_MATCHING_RULE", text: "不在只读清单内"}
	}
	if len(args) > rule.MaxArgs {
		return stageVerdict{code: "NO_MATCHING_RULE", text: "参数数量超出这条命令的只读范围"}
	}
	if bad, reason := bashPathViolationWithin(args, cwd, root); bad != "" {
		return stageVerdict{code: "PATH_OUTSIDE", text: reason}
	}
	return stageVerdict{ok: true, ruleID: rule.ID}
}
