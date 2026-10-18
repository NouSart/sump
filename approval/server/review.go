// 第二层：用户裁决。第一层（规则引擎）给不出 allow 时，命令被挂起并推到门户等用户点批准/拒绝。
//
// 边界：approval 不执行命令、不记命令历史；挂起只是一条内存里的等待项（TTL 到期即消失）。
// 挂起的是**这条命令**，不是审批服务——骨架对每次 jump 都起独立 goroutine，
// 其它审批照常处理（这也符合"零阻塞"：阻塞的只是调用方自己的那条链）。
package server

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// pendingEntry 是一条等待用户裁决的命令。
type pendingEntry struct {
	info protocol.PendingApproval
	// ch 缓冲 1：裁决只投一次，投完即走（不阻塞门户、也不阻塞本服务）。
	ch chan protocol.ApprovalDecision
}

func (s *Server) portalNow() string {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.portal
}

func (s *Server) pendingTTLNow() time.Duration {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.reviewTTL
}

func (s *Server) maxPendingNow() int {
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	return s.maxPending
}

// portalInRoster 判断门户是否在名册里。门户不在 = 没人能裁决 → 不挂起，直接按 review 返回
// （fail-closed：不把调用方的时间浪费在等一个不会到来的裁决上）。
func (s *Server) portalInRoster(name string) bool {
	if name == "" {
		return false
	}
	for _, card := range s.Roster() {
		if card.Name == name {
			return true
		}
	}
	return false
}

// registerPending 登记一条待审项；队列满时返回 false。
func (s *Server) registerPending(info protocol.PendingApproval) (*pendingEntry, bool) {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	if len(s.pending) >= s.maxPendingNow() {
		return nil, false
	}
	e := &pendingEntry{info: info, ch: make(chan protocol.ApprovalDecision, 1)}
	s.pending[info.ItemID] = e
	return e, true
}

func (s *Server) dropPending(itemID string) {
	s.pendingMu.Lock()
	delete(s.pending, itemID)
	s.pendingMu.Unlock()
}

func (s *Server) pendingSnapshot() []protocol.PendingApproval {
	s.pendingMu.Lock()
	defer s.pendingMu.Unlock()
	out := make([]protocol.PendingApproval, 0, len(s.pending))
	for _, e := range s.pending {
		out = append(out, e.info)
	}
	return out
}

// awaitUserDecision 把一条 review 命令挂起，推到门户等用户裁决。
// 返回的裁决要么是 allow（用户批准），要么是 deny（用户拒绝）；
// 超时 / 门户不在 / 队列满 时原样返回 review（调用方按"未通过"处理，不执行）。
func (s *Server) awaitUserDecision(ctx context.Context, env protocol.Envelope, result protocol.CommandApprovalResult, req protocol.CommandApprovalRequest) (protocol.CommandApprovalResult, error) {
	portal := s.portalNow()
	if !s.portalReady() {
		result.Feedback.Summary += "（门户不在线，无人裁决，本次未执行）"
		return result, nil
	}
	ttl := s.pendingTTLNow()
	now := time.Now()
	info := protocol.PendingApproval{
		ItemID: req.ItemID, Command: req.Plan.Command, WorkDir: req.Plan.WorkDir, RequestedBy: env.From,
		ReasonCode: result.ReasonCode, Summary: result.Feedback.Summary,
		Unknowns: result.Feedback.Unknowns, RuleIDs: result.Feedback.RuleIDs,
		CreatedMS: now.UnixMilli(), ExpiresMS: now.Add(ttl).UnixMilli(),
	}
	entry, ok := s.registerPending(info)
	if !ok {
		result.Feedback.Summary += "（待审队列已满，本次未执行）"
		return result, nil
	}
	defer s.dropPending(req.ItemID)

	// 推送不等待回执：门户慢/挂都不影响这条审批继续等，也不影响别的审批。
	s.Fire(portal, protocol.ApprovalPendingAction, info, env.Trace, env.Boss)
	s.logger.Printf("待审 %s 已推送门户 %s（%s；TTL %s）", req.ItemID, portal, result.ReasonCode, ttl)

	timer := time.NewTimer(ttl)
	defer timer.Stop()
	var decision protocol.ApprovalDecision
	select {
	case decision = <-entry.ch:
	case <-timer.C:
		result.Feedback.Summary += "（等待用户裁决超时，本次未执行）"
		return result, nil
	case <-ctx.Done():
		// 调用方已放弃等待：不把 ctx 错误当成裁决，也不执行。
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			result.Feedback.Summary += "（等待用户裁决超时，本次未执行）"
			return result, nil
		}
		return result, ctx.Err()
	}

	note := ""
	if decision.Note != "" {
		note = "（用户备注：" + decision.Note + "）"
	}
	if decision.Decision == protocol.DecisionAllow {
		result.Decision = protocol.DecisionAllow
		result.ReasonCode = protocol.ReasonUserApproved
		result.ValidForMS = 30000
		result.Feedback.Summary = "用户已批准执行" + note
		s.logger.Printf("待审 %s：用户批准", req.ItemID)
		return result, nil
	}
	result.Decision = protocol.DecisionDeny
	result.ReasonCode = protocol.ReasonUserDenied
	result.ValidForMS = 0
	result.Feedback.Summary = "用户拒绝执行" + note
	s.logger.Printf("待审 %s：用户拒绝", req.ItemID)
	return result, nil
}

// decide 承接用户在门户上的裁决。只接受门户发来的——裁决权不外包给任意服务。
func (s *Server) decide(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	portal := s.portalNow()
	if portal == "" || env.From != portal {
		return nil, fmt.Errorf("approval_decide: 只有门户（%s）可以裁决，收到来自 %q 的请求", portal, env.From)
	}
	var d protocol.ApprovalDecision
	if err := protocol.DecodeRaw(in.Input, &d); err != nil {
		return nil, errors.New("approval_decide: 载荷无效")
	}
	if d.ItemID == "" || len(d.ItemID) > 128 {
		return nil, errors.New("approval_decide: item_id 无效")
	}
	if d.Decision != protocol.DecisionAllow && d.Decision != protocol.DecisionDeny {
		return nil, errors.New("approval_decide: decision 只能是 allow 或 deny")
	}
	s.pendingMu.Lock()
	entry, ok := s.pending[d.ItemID]
	s.pendingMu.Unlock()
	if !ok {
		return nil, errors.New("approval_decide: 待审项不存在或已过期（可能已被裁决）")
	}
	select {
	case entry.ch <- d:
		return map[string]any{"status": "accepted", "item_id": d.ItemID, "decision": d.Decision}, nil
	default:
		// 通道里已有裁决（重复点击）：幂等接受，不覆盖第一条。
		return map[string]any{"status": "already_decided", "item_id": d.ItemID}, nil
	}
}

// list 返回当前待审项（门户重启后据此恢复列表）。
func (s *Server) list(_ context.Context, env protocol.Envelope, _ protocol.JumpPayload) (any, error) {
	portal := s.portalNow()
	if portal == "" || env.From != portal {
		return nil, fmt.Errorf("approval_list: 只有门户（%s）可以拉取待审项", portal)
	}
	return protocol.PendingApprovalList{Items: s.pendingSnapshot()}, nil
}
