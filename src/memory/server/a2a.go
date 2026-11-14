package server

// 生产 A2A 队列与 work 生命周期 method（spec §6.8/§7.4/§7.5）：
// 都是 memory 内部 method，不进 provides、不暴露给模型。

import (
	"context"
	"time"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/spv2"
)

type workCancelRequest struct {
	Work   string `msgpack:"work,omitempty" json:"work,omitempty"`
	Reason string `msgpack:"reason,omitempty" json:"reason,omitempty"`
}

type workRenewRequest struct {
	Work  string     `msgpack:"work,omitempty" json:"work,omitempty"`
	Lease spv2.Lease `msgpack:"lease" json:"lease"`
}

type messagePostRequest struct {
	Work   string   `msgpack:"work,omitempty" json:"work,omitempty"`
	To     spv2.Ref `msgpack:"to" json:"to"`
	Method string   `msgpack:"method,omitempty" json:"method,omitempty"`
	Body   string   `msgpack:"body" json:"body"`
}

type workIDRequest struct {
	Work string `msgpack:"work,omitempty" json:"work,omitempty"`
}

type messageIDRequest struct {
	ID string `msgpack:"id" json:"id"`
}

type messageNackRequest struct {
	ID          string `msgpack:"id" json:"id"`
	Error       string `msgpack:"error,omitempty" json:"error,omitempty"`
	MaxAttempts int    `msgpack:"max_attempts,omitempty" json:"max_attempts,omitempty"`
}

// authorizeOwnerRuntime 是 §6.5 的严格档：只有 owner/runtime 能 cancel/renew。
func authorizeOwnerRuntime(w spv2.Work, from spv2.Ref) error {
	switch from.Authority {
	case spv2.AuthorityOwner, spv2.AuthorityRuntime:
		return nil
	}
	if from.ID != "" && from.ID == w.Owner.ID {
		return nil
	}
	return spv2.ErrCode(spv2.CodePermissionDenied, "无权控制 work "+w.ID+"（需要 owner/runtime）", false, nil)
}

// authorizeMessage 放行 work 的参与者与 agent/owner/entry（participant 可以 message，但不能 deliver）。
func authorizeMessage(w spv2.Work, from spv2.Ref) error {
	if err := authorizeWork(w, from); err == nil {
		return nil
	}
	for _, p := range w.Participants {
		if p.Actor.ID != "" && p.Actor.ID == from.ID {
			return nil
		}
	}
	return spv2.ErrCode(spv2.CodePermissionDenied, "无权向 work "+w.ID+" 发消息", false, nil)
}

func (s *Server) nativeWorkCancel(ctx context.Context, call *spv2.Call) error {
	var req workCancelRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_cancel body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_cancel 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeOwnerRuntime(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	// §17.2 指鹿为马：终态（含已取消）不能再 cancel，必须 conflict（同态也不算幂等成功）。
	if spv2.IsTerminal(w.Status) {
		return call.ReplyError(spv2.CodeConflict, "work "+id+" 已终态（"+w.Status+"），不能 cancel", false, nil)
	}
	if err := s.cfg.Store.UpdateStatus(ctx, id, spv2.WorkCancelled); err != nil {
		return replyWorkError(call, err)
	}
	s.recordWorkTransition("cancelled")
	return call.Reply(map[string]any{"status": spv2.WorkCancelled})
}

func (s *Server) nativeWorkRenew(ctx context.Context, call *spv2.Call) error {
	var req workRenewRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_renew body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "work_renew 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeOwnerRuntime(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	if err := s.cfg.Store.Renew(ctx, id, req.Lease); err != nil {
		return replyWorkError(call, err)
	}
	rem, err := s.cfg.Store.Remaining(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(map[string]any{"remaining": rem})
}

func (s *Server) nativeMessagePost(ctx context.Context, call *spv2.Call) error {
	var req messagePostRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_post body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_post 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeMessage(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	m, err := s.cfg.Store.PostWorkMessage(ctx, id, call.Frame.From, req.To, req.Method, req.Body)
	if err != nil {
		return replyWorkError(call, err)
	}
	s.recordA2A("post")
	s.recordQueueDepth(ctx)
	return call.Reply(map[string]any{"message": m})
}

func (s *Server) nativeMessageClaim(ctx context.Context, call *spv2.Call) error {
	var req workIDRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_claim body 无效: "+err.Error(), false, nil)
	}
	id := workIDOf(call, req.Work)
	if id == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_claim 需要 work id", false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, id)
	if err != nil {
		return replyWorkError(call, err)
	}
	if err := authorizeMessage(w, call.Frame.From); err != nil {
		return replyWorkError(call, err)
	}
	m, err := s.cfg.Store.ClaimWorkMessage(ctx, id, call.Frame.From.Kind, call.Frame.From.ID, leaseTTL)
	if err != nil {
		return replyWorkError(call, err)
	}
	s.recordA2A("claim")
	s.recordQueueDepth(ctx)
	return call.Reply(map[string]any{"message": m})
}

func (s *Server) nativeMessageAck(ctx context.Context, call *spv2.Call) error {
	var req messageIDRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_ack body 无效: "+err.Error(), false, nil)
	}
	if req.ID == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_ack 需要 message id", false, nil)
	}
	if err := s.cfg.Store.AckWorkMessage(ctx, req.ID); err != nil {
		return replyWorkError(call, err)
	}
	s.recordA2A("ack")
	s.recordQueueDepth(ctx)
	return call.Reply(map[string]any{"acked": true})
}

func (s *Server) nativeMessageNack(ctx context.Context, call *spv2.Call) error {
	var req messageNackRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_nack body 无效: "+err.Error(), false, nil)
	}
	if req.ID == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "message_nack 需要 message id", false, nil)
	}
	retried, err := s.cfg.Store.NackWorkMessage(ctx, req.ID, req.Error, req.MaxAttempts)
	if err != nil {
		return replyWorkError(call, err)
	}
	s.recordA2A("nack")
	s.recordQueueDepth(ctx)
	return call.Reply(map[string]any{"retried": retried})
}

// recordWorkTransition / recordA2A 是低基数指标（spec §12.3：不放 work/actor label）。
func (s *Server) recordWorkTransition(to string) {
	recordWorkTransitionTo(s.Metrics(), to)
}

// recordWorkTransitionTo 是纯函数形态：同包单测直接喂注册表，不依赖服务骨架。
func recordWorkTransitionTo(m *spv2.Registry, to string) {
	if m == nil {
		return
	}
	m.Counter("sump_work_transitions_total", "work 状态推进次数", map[string]string{"status": to}, 1)
}

// recordQueueDepth 把 A2A 各状态计数写成 sump_queue_depth{state}（低基数，不含 work）。
func (s *Server) recordQueueDepth(ctx context.Context) {
	s.recordQueueDepthTo(s.Metrics(), ctx)
}

// recordQueueDepthTo 是纯函数形态：同包单测可以直接喂一个注册表，不依赖服务骨架。
func (s *Server) recordQueueDepthTo(m *spv2.Registry, ctx context.Context) {
	if m == nil {
		return
	}
	depth, err := s.cfg.Store.QueueDepth(ctx)
	if err != nil {
		s.logger.Printf("A2A 队列深度统计失败: %v", err)
		return
	}
	for _, state := range []string{store.WorkMsgPending, store.WorkMsgClaimed, store.WorkMsgDelivered, store.WorkMsgFailed} {
		m.Gauge("sump_queue_depth", "A2A queue depth by state", map[string]string{"state": state}, float64(depth[state]))
	}
}

func (s *Server) recordA2A(op string) {
	if m := s.Metrics(); m != nil {
		m.Counter("sump_a2a_messages_total", "A2A 队列操作次数", map[string]string{"state": op}, 1)
	}
}

// leaseTTL 是 claim 的租约时长（过期后可被重新领取）。
const leaseTTL = 30 * time.Second
