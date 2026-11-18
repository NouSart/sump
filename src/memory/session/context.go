package session

// context_base（内部 method）：会话侧上下文原材料——任务 / work / 最近记录。
// 门面拿到这些再向 experience 要记忆四节；拼装不属于本服务。

import (
	"context"
	"errors"
	"fmt"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// contextBaseResult 是 context_base 的回执：只含会话侧原始材料。
type contextBaseResult struct {
	Task    protocol.Task      `msgpack:"task"`
	Work    spv2.Work          `msgpack:"work"`
	History []protocol.Message `msgpack:"history"`
}

func (s *Server) nativeContextBase(ctx context.Context, call *spv2.Call) error {
	var p protocol.ContextPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context_base body 无效: "+err.Error(), false, nil)
	}
	from := call.Frame.From
	// spec §6.5 context 校验：from.kind 必须是 agent/service（或 owner/runtime）；
	// 已存在的 work 必须由它的 agent/owner/entry 来要上下文，否则 permission_denied。
	if from.Kind != "agent" && from.Kind != "service" &&
		from.Authority != spv2.AuthorityOwner && from.Authority != spv2.AuthorityRuntime {
		return call.ReplyError(spv2.CodePermissionDenied, "context_base: from.kind 必须是 agent/service（或 owner/runtime）", false, nil)
	}
	if w, err := s.cfg.Store.GetWork(ctx, call.Frame.Work); err == nil {
		if spv2.IsTerminal(w.Status) {
			return call.ReplyError(spv2.CodeConflict, "work "+w.ID+" 已终态（"+w.Status+"），不能再取上下文", false, nil)
		}
		if err := authorizeWork(w, from); err != nil {
			return replyWorkError(call, err)
		}
	} else if !spv2.IsCode(err, spv2.CodeNotFound) {
		return replyWorkError(call, err)
	}

	task, history, err := s.contextBase(ctx, protocol.TaskContext{
		ConversationID: p.Conversation,
		Messages:       p.Messages,
		Boss:           p.Boss,
		TaskName:       p.TaskName,
	}, metaOfCall(call))
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context_base: "+err.Error(), false, nil)
	}
	w, err := s.cfg.Store.GetWork(ctx, call.Frame.Work)
	if err != nil {
		return replyWorkError(call, err)
	}
	return call.Reply(contextBaseResult{Task: task, Work: w, History: history})
}

// contextBase 只做会话库能回答的部分：确保任务、登记 work、取最近记录。
func (s *Server) contextBase(ctx context.Context, tc protocol.TaskContext, meta callMeta) (protocol.Task, []protocol.Message, error) {
	if len(tc.Messages) == 0 {
		return protocol.Task{}, nil, errors.New("context.messages 为空")
	}
	cid := tc.ConversationID
	if cid == "" {
		cid = "default"
	}
	newName := protocol.ClampTaskName(tc.TaskName)
	if newName == "" {
		newName = taskNameFrom(recallQuery(tc.Messages))
	}
	task, err := s.cfg.Store.EnsureTask(cid, newName)
	if err != nil {
		return protocol.Task{}, nil, fmt.Errorf("任务解析失败: %w", err)
	}
	if err := s.ensureContextWork(ctx, meta, tc, task.Name); err != nil {
		return protocol.Task{}, nil, err
	}
	history, err := s.cfg.Store.Recent(cid, s.currentHistoryLimit(), task.ID)
	if err != nil {
		return protocol.Task{}, nil, fmt.Errorf("查询历史失败: %w", err)
	}
	return task, history, nil
}
