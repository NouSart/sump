package server

// context（spec §7.2）：reasoner 同步向 memory 要上下文——不再用 recall+resume 两跳。
// 返回组装后的消息与 work 行；work 在 memory 唯一登记（§1.6），重复调用幂等。

import (
	"context"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) nativeContext(ctx context.Context, call *spv2.Call) error {
	var p protocol.ContextPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context body 无效: "+err.Error(), false, nil)
	}
	tc := protocol.TaskContext{
		ConversationID: p.Conversation,
		Messages:       p.Messages,
		Boss:           p.Boss,
		TaskName:       p.TaskName,
	}
	from := call.Frame.From
	// spec §6.5 context 校验：from.kind 必须是 agent/service（或 owner/runtime）；
	// 已存在的 work 必须由它的 agent/owner/entry 来要上下文，否则 permission_denied。
	if from.Kind != "agent" && from.Kind != "service" &&
		from.Authority != spv2.AuthorityOwner && from.Authority != spv2.AuthorityRuntime {
		return call.ReplyError(spv2.CodePermissionDenied, "context: from.kind 必须是 agent/service（或 owner/runtime）", false, nil)
	}
	if w, err := s.cfg.Store.GetWork(ctx, call.Frame.Work); err == nil {
		if spv2.IsTerminal(w.Status) {
			return call.ReplyError(spv2.CodeConflict, "work "+w.ID+" 已终态（"+w.Status+"），不能再取 context", false, nil)
		}
		if err := authorizeWork(w, from); err != nil {
			return replyWorkError(call, err)
		}
	} else if !spv2.IsCode(err, spv2.CodeNotFound) {
		return replyWorkError(call, err)
	}
	task, final, err := s.assemble(ctx, tc, metaOfCall(call))
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context: "+err.Error(), false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, call.Frame.Work)
	if err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(map[string]any{"messages": final, "work": w, "task": task})
}
