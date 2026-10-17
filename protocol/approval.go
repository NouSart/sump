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
