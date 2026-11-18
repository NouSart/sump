package facade

// context 模块：没有数据权威，只做“取会话基线 + 取经验四节 + 插入/裁剪”。
// 两个内部调用都走 SPv2，帧头保留原调用方，work 与外部请求一致。

import (
	"context"
	"strings"

	"github.com/NouSart/sump/memory/facade/compose"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	"github.com/vmihailenco/msgpack/v5"
)

type contextBaseResult struct {
	Task    protocol.Task      `msgpack:"task"`
	Work    spv2.Work          `msgpack:"work"`
	History []protocol.Message `msgpack:"history"`
}

func (s *Server) nativeContext(ctx context.Context, call *spv2.Call) error {
	var p protocol.ContextPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context body 无效: "+err.Error(), false, nil)
	}
	from := call.Frame.From
	if from.Kind != "agent" && from.Kind != "service" &&
		from.Authority != spv2.AuthorityOwner && from.Authority != spv2.AuthorityRuntime {
		return call.ReplyError(spv2.CodePermissionDenied, "context: from.kind 必须是 agent/service（或 owner/runtime）", false, nil)
	}
	if len(p.Messages) == 0 {
		return call.ReplyError(spv2.CodeInvalidArgument, "context: context.messages 为空", false, nil)
	}
	baseRaw, err := s.CallWithRef(ctx, from, "session", "context_base",
		msgpack.RawMessage(call.Frame.Body), call.Frame.Work)
	if err != nil {
		return replyForwardError(call, "session", "context_base", err)
	}
	var base contextBaseResult
	if err := protocol.DecodeRaw(baseRaw, &base); err != nil {
		return call.ReplyError(spv2.CodeInternal, "context_base 回执无法解码: "+err.Error(), true, nil)
	}

	cid := p.Conversation
	if cid == "" {
		cid = "default"
	}
	injectRaw, err := s.CallWithRef(ctx, from, "experience", "inject", protocol.InjectPayload{
		Viewer: from.ID,
		Origin: cid,
		Query:  recallQuery(p.Messages),
	}, call.Frame.Work)
	if err != nil {
		return replyForwardError(call, "experience", "inject", err)
	}
	var injected protocol.InjectResult
	if err := protocol.DecodeRaw(injectRaw, &injected); err != nil {
		return call.ReplyError(spv2.CodeInternal, "inject 回执无法解码: "+err.Error(), true, nil)
	}

	history := compose.TrimOverlap(base.History, compose.ConversationTail(p.Messages))
	final := compose.Inject(p.Messages, compose.BuildMemory(history,
		injected.Core, injected.Relevant, injected.Experience, injected.Others))
	return call.Reply(map[string]any{"messages": final, "work": base.Work, "task": base.Task})
}

// recallQuery 取最后一条非空 user 消息作为召回查询。
func recallQuery(messages []protocol.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	return ""
}
