// Package review 是第二层钩子的契约。首版不注册人工批准动作，不驻留待审工作流。
package review

import (
	"context"
	"github.com/NouSart/sump/protocol"
)

type Context struct {
	Request  protocol.CommandApprovalRequest
	Decision protocol.CommandApprovalResult
	Trace    string
	Boss     string
}

type Explanation struct {
	ItemID     string
	PlanDigest string
	Purpose    string
	Effects    []string
	Unknowns   []string
}

// 后续第二层：ExplainPort 与 UserNoticePort 并行调用；解释回传后调用 UserUpdatePort。
// 当前只保留接口，不能将缺失实现视为用户同意。
type ExplainPort interface {
	RequestExplanation(context.Context, Context) error
}
type UserNoticePort interface {
	NotifyPending(context.Context, Context) error
}
type ExplanationReceiver interface {
	ReceiveExplanation(context.Context, Context, Explanation) error
}
type UserUpdatePort interface {
	UpdateExplanation(context.Context, Context, Explanation) error
}
