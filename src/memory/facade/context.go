package facade

// context 模块：session 负责基座（四态 + 快照），门面负责 experience 注入。
//
// R-026-D1=(a)：session 的快照只存基座（messages / task / work），experience 每次现注入。
// 处理顺序固定：
//  1. 解码 ContextRequest（形状错误在门面就拒绝）；
//  2. 原样转发给 session.context_base（授权 / 触发状态 / 快照都在 session）；
//  3. completed 直通——不读快照、不调 experience（R-026-D2）；
//  4. 其余：用基座最后一条 user 消息做召回查询，取 experience 四节，注入后返回。
//
// 快照语义因此是「基座冻结」：assembled 重取时 messages/task/work 逐字不变，
// 【记忆】段可能随 experience 现算结果变化；调用方不得把它当成整份响应的冻结。

import (
	"context"
	"strings"

	"github.com/NouSart/sump/memory/facade/compose"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	"github.com/vmihailenco/msgpack/v5"
)

func (s *Server) nativeContext(ctx context.Context, call *spv2.Call) error {
	var req protocol.ContextRequest
	if err := protocol.DecodeRaw(call.Frame.Body, &req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context body 无效: "+err.Error(), false, nil)
	}
	from := call.Frame.From
	baseRaw, err := s.CallWithRef(ctx, from, "session", "context_base",
		msgpack.RawMessage(call.Frame.Body), call.Frame.Work)
	if err != nil {
		return replyForwardError(call, "session", "context_base", err)
	}
	var resp protocol.ContextResponse
	if err := protocol.DecodeRaw(baseRaw, &resp); err != nil {
		return call.ReplyError(spv2.CodeInternal, "context_base 回执无法解码: "+err.Error(), true, nil)
	}
	// completed：本轮已经收口。只补 ACK，不读快照、不注入 experience。
	if resp.Completed {
		return call.Reply(resp)
	}

	cid := strings.TrimSpace(resp.Task.ConversationID)
	if cid == "" {
		cid = "default"
	}
	injectRaw, err := s.CallWithRef(ctx, from, "experience", "inject", protocol.InjectPayload{
		Viewer: from.ID,
		Origin: cid,
		Query:  recallQuery(resp.Messages),
	}, call.Frame.Work)
	if err != nil {
		return replyForwardError(call, "experience", "inject", err)
	}
	var injected protocol.InjectResult
	if err := protocol.DecodeRaw(injectRaw, &injected); err != nil {
		return call.ReplyError(spv2.CodeInternal, "inject 回执无法解码: "+err.Error(), true, nil)
	}

	memoryText := compose.BuildMemory(nil,
		injected.Core, injected.Relevant, injected.Experience, injected.Others)
	resp.Messages = compose.Inject(resp.Messages, memoryText)
	return call.Reply(resp)
}

// recallQuery 取最后一条非空 user 消息作为召回查询（基座里历史已按 seq 排好）。
func recallQuery(messages []protocol.LLMMessage) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	return ""
}
