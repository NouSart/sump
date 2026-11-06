package spv2

import (
	"fmt"
	"time"
)

// Work class。
const (
	ClassRequest = "request"
	ClassThread  = "thread"
	ClassSpawn   = "spawn"
)

// Work status。
const (
	WorkOpen      = "open"
	WorkRunning   = "running"
	WorkWaiting   = "waiting"
	WorkStalled   = "stalled"
	WorkDone      = "done"
	WorkFailed    = "failed"
	WorkCancelled = "cancelled"
	WorkOrphaned  = "orphaned"
)

// Lease 是 work 的预算与截止时间。
type Lease struct {
	DeadlineAt   string `msgpack:"deadline_at,omitempty" json:"deadline_at,omitempty"`
	MaxToolCalls int    `msgpack:"max_tool_calls,omitempty" json:"max_tool_calls,omitempty"`
	MaxTokens    int    `msgpack:"max_tokens,omitempty" json:"max_tokens,omitempty"`
	MaxApprovals int    `msgpack:"max_approvals,omitempty" json:"max_approvals,omitempty"`
	MaxChildren  int    `msgpack:"max_children,omitempty" json:"max_children,omitempty"`
	MaxDepth     int    `msgpack:"max_depth,omitempty" json:"max_depth,omitempty"`
}

// DefaultLease 返回 SPv2.1 默认租约。
func DefaultLease(now time.Time) Lease {
	return Lease{
		DeadlineAt:   now.Add(10 * time.Minute).UTC().Format(time.RFC3339),
		MaxToolCalls: 50,
		MaxTokens:    100000,
		MaxApprovals: 5,
		MaxChildren:  8,
		MaxDepth:     4,
	}
}

// Charge 是一次租约扣减。
type Charge struct {
	ToolCalls int `msgpack:"tool_calls,omitempty" json:"tool_calls,omitempty"`
	Tokens    int `msgpack:"tokens,omitempty" json:"tokens,omitempty"`
	Approvals int `msgpack:"approvals,omitempty" json:"approvals,omitempty"`
	Children  int `msgpack:"children,omitempty" json:"children,omitempty"`
	Depth     int `msgpack:"depth,omitempty" json:"depth,omitempty"`
}

// Remaining 是租约余额。
type Remaining struct {
	DeadlineAt string `msgpack:"deadline_at,omitempty" json:"deadline_at,omitempty"`
	ToolCalls  int    `msgpack:"tool_calls" json:"tool_calls"`
	Tokens     int    `msgpack:"tokens" json:"tokens"`
	Approvals  int    `msgpack:"approvals" json:"approvals"`
	Children   int    `msgpack:"children" json:"children"`
	Depth      int    `msgpack:"depth" json:"depth"`
}

// Participant 是 work 的参与者。
type Participant struct {
	Actor  Ref    `msgpack:"actor" json:"actor"`
	Role   string `msgpack:"role" json:"role"`
	Status string `msgpack:"status" json:"status"`
}

// Work 是 SPv2.1 的工作对象。
type Work struct {
	ID           string         `msgpack:"id" json:"id"`
	Parent       string         `msgpack:"parent,omitempty" json:"parent,omitempty"`
	Root         string         `msgpack:"root" json:"root"`
	Class        string         `msgpack:"class" json:"class"`
	Owner        Ref            `msgpack:"owner" json:"owner"`
	Agent        Ref            `msgpack:"agent" json:"agent"`
	Entry        Ref            `msgpack:"entry" json:"entry"`
	Goal         string         `msgpack:"goal" json:"goal"`
	Status       string         `msgpack:"status" json:"status"`
	Lease        Lease          `msgpack:"lease" json:"lease"`
	Participants []Participant  `msgpack:"participants,omitempty" json:"participants,omitempty"`
	Policy       map[string]any `msgpack:"policy,omitempty" json:"policy,omitempty"`
	CreatedAt    string         `msgpack:"created_at" json:"created_at"`
	UpdatedAt    string         `msgpack:"updated_at" json:"updated_at"`
	ClosedAt     string         `msgpack:"closed_at,omitempty" json:"closed_at,omitempty"`
}

// Validate 校验 work 的必填项。
func (w Work) Validate() error {
	if w.ID == "" {
		return fmt.Errorf("work.id 必填")
	}
	if w.Root == "" {
		return fmt.Errorf("work.root 必填")
	}
	switch w.Class {
	case ClassRequest, ClassThread, ClassSpawn:
	default:
		return fmt.Errorf("work.class 非法: %q", w.Class)
	}
	if w.Owner.Kind == "" || w.Owner.ID == "" {
		return fmt.Errorf("work.owner 必填")
	}
	if w.Agent.Kind == "" || w.Agent.ID == "" {
		return fmt.Errorf("work.agent 必填")
	}
	switch w.Status {
	case WorkOpen, WorkRunning, WorkWaiting, WorkStalled, WorkDone, WorkFailed, WorkCancelled, WorkOrphaned:
	default:
		return fmt.Errorf("work.status 非法: %q", w.Status)
	}
	return nil
}

// IsTerminal 判断 work 是否终态。
func IsTerminal(status string) bool {
	switch status {
	case WorkDone, WorkFailed, WorkCancelled:
		return true
	default:
		return false
	}
}

// CanTransition 判断状态转移是否合法。
func CanTransition(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case WorkOpen:
		return to == WorkRunning || to == WorkCancelled || to == WorkStalled
	case WorkRunning:
		return to == WorkWaiting || to == WorkDone || to == WorkFailed || to == WorkCancelled || to == WorkStalled
	case WorkWaiting:
		return to == WorkRunning || to == WorkCancelled || to == WorkStalled || to == WorkOrphaned
	case WorkStalled:
		return to == WorkRunning || to == WorkCancelled
	case WorkOrphaned:
		return to == WorkCancelled || to == WorkDone
	default:
		return false
	}
}
