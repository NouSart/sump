package protocol

// 本文件只定义命令审批的消息契约；不导入任何服务的实现。
import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"hash"
	"strconv"
)

const (
	ApprovalVersion   = 1
	ApprovalAction    = "approval_request"
	PowerShellProfile = "powershell-clean-v1"
	DecisionAllow     = "allow"
	DecisionDeny      = "deny"
	DecisionReview    = "review"
)

// 第二层（用户裁决）的动作名与裁决码。命名见 docs/concepts/protocol.md 第 5 节「动作词汇与工具暴露」。
const (
	// ApprovalPendingAction 是 approval → 门户的推送：一条待审命令（不等待回执）。
	ApprovalPendingAction = "approval_pending"
	// ApprovalDecideAction 是门户 → approval 的裁决：用户点了批准或拒绝。
	// 只接受门户发来的裁决（approval 侧按 from 校验），别的服务调一律拒绝。
	ApprovalDecideAction = "approval_decide"
	// ApprovalListAction 是门户 → approval 的全量拉取：门户重启后据此恢复待审列表。
	ApprovalListAction = "approval_list"

	// ReasonUserApproved / ReasonUserDenied 是第二层裁决的理由码（第一层是 RULE_ALLOW / POLICY_DENY 等）。
	ReasonUserApproved = "USER_APPROVED"
	ReasonUserDenied   = "USER_DENIED"
)

// PendingApproval 是推给门户的一条待审命令。命令原文必须可见——用户要据此判断；
// 这里不含任何密钥或环境变量（shell 计划里只有命令行、工作目录与固定启动参数）。
type PendingApproval struct {
	ItemID      string   `json:"item_id" msgpack:"item_id"`
	Command     string   `json:"command" msgpack:"command"`
	WorkDir     string   `json:"work_dir,omitempty" msgpack:"work_dir,omitempty"`
	RequestedBy string   `json:"requested_by,omitempty" msgpack:"requested_by,omitempty"`
	ReasonCode  string   `json:"reason_code,omitempty" msgpack:"reason_code,omitempty"`
	Summary     string   `json:"summary,omitempty" msgpack:"summary,omitempty"`
	Unknowns    []string `json:"unknowns,omitempty" msgpack:"unknowns,omitempty"`
	RuleIDs     []string `json:"rule_ids,omitempty" msgpack:"rule_ids,omitempty"`
	CreatedMS   int64    `json:"created_ms" msgpack:"created_ms"`
	ExpiresMS   int64    `json:"expires_ms" msgpack:"expires_ms"`
}

// ApprovalDecision 是用户在门户上的一条裁决：allow = 批准执行，deny = 拒绝。
type ApprovalDecision struct {
	ItemID   string `json:"item_id" msgpack:"item_id"`
	Decision string `json:"decision" msgpack:"decision"`
	Note     string `json:"note,omitempty" msgpack:"note,omitempty"`
}

// PendingApprovalList 是 approval_list 的应答。
type PendingApprovalList struct {
	Items []PendingApproval `json:"items" msgpack:"items"`
}

// ExecutionPlan 是执行方固定的执行描述。Args 不含末尾原始命令。
// Profile 是执行方的环境约定，不是身份认证或沙箱证明。
type ExecutionPlan struct {
	Version        int      `json:"version" msgpack:"version"`
	Command        string   `json:"command" msgpack:"command"`
	WorkDir        string   `json:"work_dir" msgpack:"work_dir"`
	ShellPath      string   `json:"shell_path" msgpack:"shell_path"`
	Args           []string `json:"args" msgpack:"args"`
	Dialect        string   `json:"dialect" msgpack:"dialect"`
	Platform       string   `json:"platform" msgpack:"platform"`
	Profile        string   `json:"profile" msgpack:"profile"`
	TimeoutMS      int64    `json:"timeout_ms" msgpack:"timeout_ms"`
	MaxOutputBytes int64    `json:"max_output_bytes" msgpack:"max_output_bytes"`
}

// Digest 使用固定字段顺序、uint64 大端长度前缀和 UTF-8 字节，避免 map 顺序差异。
// 摘要绑定描述，不认证调用方，也不冻结磁盘、PATH 或脚本内容。
func (p ExecutionPlan) Digest() string {
	h := sha256.New()
	for _, s := range []string{"sump-execution-plan-v1", strconv.Itoa(p.Version), p.Command,
		p.WorkDir, p.ShellPath, p.Dialect, p.Platform, p.Profile,
		strconv.FormatInt(p.TimeoutMS, 10), strconv.FormatInt(p.MaxOutputBytes, 10), strconv.Itoa(len(p.Args))} {
		writeDigestPart(h, s)
	}
	for _, arg := range p.Args {
		writeDigestPart(h, arg)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeDigestPart(h hash.Hash, s string) {
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len([]byte(s))))
	_, _ = h.Write(n[:])
	_, _ = h.Write([]byte(s))
}

type CommandApprovalRequest struct {
	Version int           `json:"schema_version" msgpack:"schema_version"`
	ItemID  string        `json:"item_id" msgpack:"item_id"`
	Plan    ExecutionPlan `json:"plan" msgpack:"plan"`
}

// ModelFeedback 是同一裁决的可读投影，由 shell 唯一负责送入 report_result。
type ModelFeedback struct {
	Summary  string   `json:"summary" msgpack:"summary"`
	Unknowns []string `json:"unknowns,omitempty" msgpack:"unknowns,omitempty"`
	RuleIDs  []string `json:"rule_ids,omitempty" msgpack:"rule_ids,omitempty"`
}

type CommandApprovalResult struct {
	Version       int           `json:"schema_version" msgpack:"schema_version"`
	ItemID        string        `json:"item_id" msgpack:"item_id"`
	DecisionID    string        `json:"decision_id" msgpack:"decision_id"`
	Decision      string        `json:"decision" msgpack:"decision"`
	PlanDigest    string        `json:"plan_digest" msgpack:"plan_digest"`
	PolicyVersion string        `json:"policy_version" msgpack:"policy_version"`
	ReasonCode    string        `json:"reason_code" msgpack:"reason_code"`
	ValidForMS    int64         `json:"valid_for_ms" msgpack:"valid_for_ms"`
	Feedback      ModelFeedback `json:"llm_feedback" msgpack:"llm_feedback"`
}

// ValidateFor 在执行边界使用：错误或不完整响应绝不被解释为通过。
func (r CommandApprovalResult) ValidateFor(req CommandApprovalRequest) error {
	if r.Version != ApprovalVersion || r.ItemID != req.ItemID || r.PlanDigest != req.Plan.Digest() {
		return errors.New("审批响应版本、任务或执行摘要不匹配")
	}
	if r.DecisionID == "" || r.PolicyVersion == "" || r.ReasonCode == "" || r.Feedback.Summary == "" {
		return errors.New("审批响应缺少裁决依据")
	}
	if r.Decision != DecisionAllow && r.Decision != DecisionDeny && r.Decision != DecisionReview {
		return errors.New("审批返回未知 decision")
	}
	if r.Decision == DecisionAllow && (r.ValidForMS <= 0 || r.ValidForMS > 30000) {
		return errors.New("审批许可有效期无效")
	}
	return nil
}
