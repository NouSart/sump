// 审批接缝：审批是 shell 自己的活儿（谁裁、按什么协议裁由审批服务定义），
// 但执行前必须先拿到裁决——未通过就不执行。
//
// 现在只有一种实现：未接入（FailClosed）——一律“未通过”。
// 队友把审批服务的协议定下来后，实现本文件的 Approver 接口（或在 Config 注入自己的实现）即可，
// 其余代码不用动：裁决只有一个入口（process → Approver.Approve），一个出口（补报 rejected 的原因原文）。
package server

import (
	"context"
	"errors"
)

// Approver 裁决一条待执行命令：nil = 通过（开始执行）；否则返回值即拒绝原因（原样补报给调用方）。
type Approver interface {
	Approve(ctx context.Context, req ApprovalRequest) error
}

// ApprovalRequest 是送审的一条命令。
type ApprovalRequest struct {
	ItemID  string // 受理句柄
	Command string // 命令行原文
	WorkDir string // 已校验的工作目录（绝对路径）
	Service string // 审批服务名（审批接缝里读自设置 approval.service；空 = 未接入）
}

// FailClosed 是未接入审批时的默认裁决：一律未通过。
// 姿态取最保守：没有裁决就说没有裁决，命令不执行。
type FailClosed struct{}

// Approve 一律拒绝。
func (FailClosed) Approve(context.Context, ApprovalRequest) error {
	return errors.New("未接入审批服务（approval.service 为空），命令不执行")
}

// approver 返回当前生效的裁决实现（未注入 = FailClosed）。
func (s *Server) approver() Approver {
	if s.cfg.Approver != nil {
		return s.cfg.Approver
	}
	return FailClosed{}
}
