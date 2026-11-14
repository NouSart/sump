package server

// SPv2.1 原生入口：/spv2 的 call 直接解 body，调用与共享实现。
//
// 这里不合成 v1 信封、也不构造跳转载荷：入参就是帧的 body，
// 出参走 call.Reply / call.ReplyError。

import (
	"context"

	"github.com/NouSart/sump/spv2"
)

// registerNative 注册全部动作的原生 method（New 里调用）。
// Tool 与 provides 里的 Tool 对齐：只有 dig / remember / shared_sessions 是工具。
func (s *Server) registerNative() {
	s.HandleNative(spv2.Method{
		Name:        "store",
		Description: "记一条会话记录（同一任务下与上一条完全相同则幂等跳过）",
		Input: objectSchema(map[string]any{
			"conversation_id": stringSchema(),
			"boss":            stringSchema(),
			"role":            stringSchema(),
			"content":         stringSchema(),
			"kind":            stringSchema(),
			"meta":            stringSchema(),
			"seq":             integerSchema(),
			"task_id":         stringSchema(),
			"task_name":       stringSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeStore)
	s.HandleNative(spv2.Method{
		Name:        "history",
		Description: "取某会话最近的记录（按时间正序；供界面回填）",
		Input: objectSchema(map[string]any{
			"conversation_id": stringSchema(),
			"limit":           integerSchema(),
			"after_seq":       integerSchema(),
			"task_id":         stringSchema(),
			"all_tasks":       booleanSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeHistory)
	s.HandleNative(spv2.Method{
		Name:        "conversation_list",
		Description: "列会话（按最近活跃排序；不暴露给模型）",
		Input: objectSchema(map[string]any{
			"limit":            integerSchema(),
			"include_archived": booleanSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeConversationList)
	s.HandleNative(spv2.Method{
		Name:        "conversation_rename",
		Description: "改会话名（空标题 = 清掉名字；不暴露给模型）",
		Input: objectSchema(map[string]any{
			"id":    stringSchema(),
			"title": stringSchema(),
		}, "id"),
		Output: objectSchema(nil),
	}, s.nativeConversationRename)
	s.HandleNative(spv2.Method{
		Name:        "note",
		Description: "写一条经验（整理时机由调用方决定；不暴露给模型）",
		Input: objectSchema(map[string]any{
			"content":         stringSchema(),
			"conversation_id": stringSchema(),
			"task_id":         stringSchema(),
			"durability":      stringSchema(),
		}, "content"),
		Output: objectSchema(nil),
	}, s.nativeNote)
	s.HandleNative(spv2.Method{
		Name:        "dig",
		Tool:        true,
		Description: "顺着线头往下扒：entry_id 翻同簇兄弟，node_id 展开整个大类",
		Input: objectSchema(map[string]any{
			"entry_id": stringSchema(),
			"node_id":  stringSchema(),
			"limit":    integerSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeDig)
	s.HandleNative(spv2.Method{
		Name:        "lineheads",
		Description: "直接要经验线头（门户 / 调试用；recall 里自动带）",
		Input: objectSchema(map[string]any{
			"query": stringSchema(),
			"limit": integerSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeLineHeads)
	s.HandleNative(spv2.Method{
		Name:        "prune",
		Description: "经验网软剪枝（只标状态、只断干枯的线，不删行）",
		Input:       objectSchema(nil),
		Output:      objectSchema(nil),
	}, s.nativeNetPrune)
	s.HandleNative(spv2.Method{
		Name:        "remember",
		Tool:        true,
		Description: "写一条长期记忆；priority>0 = 核心记忆（每次对话都带上）",
		Input: objectSchema(map[string]any{
			"kind":            stringSchema(),
			"content":         stringSchema(),
			"priority":        integerSchema(),
			"conversation_id": stringSchema(),
		}, "kind", "content"),
		Output: objectSchema(nil),
	}, s.nativeRemember)
	s.HandleNative(spv2.Method{
		Name:        "forget",
		Description: "软删一条长期记忆（不暴露给模型）",
		Input: objectSchema(map[string]any{
			"id": integerSchema(),
		}, "id"),
		Output: objectSchema(nil),
	}, s.nativeForget)
	s.HandleNative(spv2.Method{
		Name:        "publish_session",
		Description: "发布一次任务的摘要（复盘调用；不暴露给模型）",
		Input: objectSchema(map[string]any{
			"conversation_id": stringSchema(),
			"task_id":         stringSchema(),
			"title":           stringSchema(),
			"goal":            stringSchema(),
			"outcome":         stringSchema(),
			"capabilities":    stringArraySchema(),
			"private":         booleanSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativePublishSession)
	s.HandleNative(spv2.Method{
		Name:        "shared_sessions",
		Tool:        true,
		Description: "看别的会话在做什么（只给摘要，看不到它们的对话）",
		Input: objectSchema(map[string]any{
			"limit":   integerSchema(),
			"exclude": stringSchema(),
			"query":   stringSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeSharedSessions)
	s.HandleNative(spv2.Method{
		Name:        "shared_forget",
		Description: "删共享物（不暴露给模型）",
		Input: objectSchema(map[string]any{
			"session_id": stringSchema(),
			"task_id":    stringSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeSharedForget)
	s.HandleNative(spv2.Method{
		Name:        "conversation_open",
		Description: "幂等建会话（不暴露给模型）",
		Input: objectSchema(map[string]any{
			"id":    stringSchema(),
			"title": stringSchema(),
			"entry": stringSchema(),
		}, "id"),
		Output: objectSchema(nil),
	}, s.nativeConversationOpen)
	s.HandleNative(spv2.Method{
		Name:        "conversation_archive",
		Description: "归档 / 取消归档会话（归档 = 不进默认列表，不拦写入；不暴露给模型）",
		Input: objectSchema(map[string]any{
			"id":       stringSchema(),
			"archived": booleanSchema(),
		}, "id"),
		Output: objectSchema(nil),
	}, s.nativeConversationArchive)
	s.HandleNative(spv2.Method{
		Name:        "task_current",
		Description: "取当前任务（没有就是空）",
		Input: objectSchema(map[string]any{
			"conversation_id": stringSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeTaskCurrent)
	s.HandleNative(spv2.Method{
		Name:        "task_open",
		Description: "新开一个进行中的任务",
		Input: objectSchema(map[string]any{
			"conversation_id": stringSchema(),
			"name":            stringSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeTaskOpen)
	s.HandleNative(spv2.Method{
		Name:        "task_finish",
		Description: "完结任务（可顺带把名字改准）",
		Input: objectSchema(map[string]any{
			"task_id": stringSchema(),
			"name":    stringSchema(),
		}, "task_id"),
		Output: objectSchema(nil),
	}, s.nativeTaskFinish)
	s.HandleNative(spv2.Method{
		Name:        "task_resume",
		Description: "恢复一个未完结任务：顶成当前任务，并把这一段记录一并带走",
		Input: objectSchema(map[string]any{
			"task_id": stringSchema(),
		}, "task_id"),
		Output: objectSchema(nil),
	}, s.nativeTaskResume)
	s.HandleNative(spv2.Method{
		Name:        "task_rename",
		Description: "改任务名（不改状态）",
		Input: objectSchema(map[string]any{
			"task_id": stringSchema(),
			"name":    stringSchema(),
		}, "task_id"),
		Output: objectSchema(nil),
	}, s.nativeTaskRename)
	s.HandleNative(spv2.Method{
		Name:        "task_list",
		Description: "列出会话的任务（最近活跃在前）",
		Input: objectSchema(map[string]any{
			"conversation_id": stringSchema(),
			"limit":           integerSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeTaskList)
	s.HandleNative(spv2.Method{
		Name:        "context",
		Description: "同步返回组装后的上下文与 work 行（spec §7.2；替代 recall+resume 两跳）",
		Input: objectSchema(map[string]any{
			"conversation": stringSchema(),
			"messages":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"boss":         stringSchema(),
			"task_name":    stringSchema(),
		}, "messages"),
		Output: objectSchema(map[string]any{"messages": map[string]any{"type": "array"}, "work": map[string]any{"type": "object"}}),
	}, s.nativeContext)
	// work 账本（spec §6.8 内部 method）：给入口/reasoner 内部用，不进 provides、不暴露给模型。
	s.HandleNative(spv2.Method{
		Name:        "work_get",
		Description: "读 work 行与租约余额（内部）",
		Input: objectSchema(map[string]any{
			"work": stringSchema(),
		}),
		Output: objectSchema(nil),
	}, s.nativeWorkGet)
	s.HandleNative(spv2.Method{
		Name:        "lease_charge",
		Description: "原子扣减 work 租约（内部；预算不足/过期/已 stalled 分别回 §8 错误码）",
		Input: objectSchema(map[string]any{
			"work": stringSchema(),
			"delta": objectSchema(map[string]any{
				"tool_calls": integerSchema(),
				"tokens":     integerSchema(),
				"approvals":  integerSchema(),
				"children":   integerSchema(),
				"depth":      integerSchema(),
			}),
		}),
		Output: objectSchema(nil),
	}, s.nativeLeaseCharge)
	s.HandleNative(spv2.Method{
		Name:        "lease_release",
		Description: "归还 work 租约（内部；不超过初始上限）",
		Input: objectSchema(map[string]any{
			"work": stringSchema(),
			"delta": objectSchema(map[string]any{
				"tool_calls": integerSchema(),
				"tokens":     integerSchema(),
				"approvals":  integerSchema(),
				"children":   integerSchema(),
				"depth":      integerSchema(),
			}),
		}),
		Output: objectSchema(nil),
	}, s.nativeLeaseRelease)
	s.HandleNative(spv2.Method{
		Name:        "work_update",
		Description: "推进 work 状态（内部；按 §6.3 状态机，非法转移回 conflict）",
		Input: objectSchema(map[string]any{
			"work":   stringSchema(),
			"status": stringSchema(),
		}, "status"),
		Output: objectSchema(nil),
	}, s.nativeWorkUpdate)
	s.HandleNative(spv2.Method{
		Name:        "work_sweep",
		Description: "手动执行一次 work sweep（内部/运维用；周期 sweep 默认 30s）",
		Input:       objectSchema(nil),
		Output:      objectSchema(nil),
	}, s.nativeWorkSweep)
	// work 生命周期与 A2A 队列（spec §6.8/§7.4/§7.5）：内部 method，不进 provides。
	s.HandleNative(spv2.Method{
		Name:        "work_cancel",
		Description: "取消 work（owner/runtime）；终态回 conflict",
		Input: objectSchema(map[string]any{
			"work":   stringSchema(),
			"reason": stringSchema(),
		}, "work"),
		Output: objectSchema(nil),
	}, s.nativeWorkCancel)
	s.HandleNative(spv2.Method{
		Name:        "work_renew",
		Description: "续租 work（owner/runtime）：刷新 deadline/budget，stalled 拉回 running",
		Input: objectSchema(map[string]any{
			"work":  stringSchema(),
			"lease": map[string]any{"type": "object"},
		}, "work"),
		Output: objectSchema(nil),
	}, s.nativeWorkRenew)
	s.HandleNative(spv2.Method{
		Name:        "message_post",
		Description: "A2A 入队一条消息（正文只存一份，work_messages 存 body_ref）",
		Input: objectSchema(map[string]any{
			"work":   stringSchema(),
			"to":     map[string]any{"type": "object"},
			"method": stringSchema(),
			"body":   stringSchema(),
		}, "to", "body"),
		Output: objectSchema(nil),
	}, s.nativeMessagePost)
	s.HandleNative(spv2.Method{
		Name:        "message_claim",
		Description: "领取一条 A2A 消息（pending/租约过期的 claimed；to 不匹配的领不到）",
		Input: objectSchema(map[string]any{
			"work": stringSchema(),
		}, "work"),
		Output: objectSchema(nil),
	}, s.nativeMessageClaim)
	s.HandleNative(spv2.Method{
		Name:        "message_ack",
		Description: "确认 A2A 消息（claimed→delivered；状态不对回 conflict）",
		Input: objectSchema(map[string]any{
			"id": stringSchema(),
		}, "id"),
		Output: objectSchema(nil),
	}, s.nativeMessageAck)
	s.HandleNative(spv2.Method{
		Name:        "message_nack",
		Description: "A2A 消息处理失败：未超上限回 pending，超过进 failed（dead-letter）",
		Input: objectSchema(map[string]any{
			"id":           stringSchema(),
			"error":        stringSchema(),
			"max_attempts": integerSchema(),
		}, "id"),
		Output: objectSchema(nil),
	}, s.nativeMessageNack)
}

// ---------- 原生 handler ----------
//
// 每个 handler 只做三件事：把 call.Frame.Body 交给共享实现、把帧元信息翻成 callMeta、
// 用 call.Reply / call.ReplyError 回应。业务语义全在共享实现里。

// metaOfCall 把 SPv2 帧翻成动作需要的调用元信息。
func metaOfCall(call *spv2.Call) callMeta {
	return callMeta{from: call.Frame.From.ID, trace: call.Frame.Work}
}

func (s *Server) nativeStore(_ context.Context, call *spv2.Call) error {
	out, err := s.store(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeHistory(_ context.Context, call *spv2.Call) error {
	out, err := s.history(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeConversationList(_ context.Context, call *spv2.Call) error {
	out, err := s.conversationList(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeConversationRename(_ context.Context, call *spv2.Call) error {
	out, err := s.conversationRename(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeNote(ctx context.Context, call *spv2.Call) error {
	out, err := s.note(ctx, call.Frame.Body, metaOfCall(call))
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeDig(_ context.Context, call *spv2.Call) error {
	out, err := s.dig(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeLineHeads(ctx context.Context, call *spv2.Call) error {
	out, err := s.lineHeads(ctx, call.Frame.Body, metaOfCall(call))
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeNetPrune(_ context.Context, call *spv2.Call) error {
	out, err := s.netPrune(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeRemember(ctx context.Context, call *spv2.Call) error {
	out, err := s.remember(ctx, call.Frame.Body, metaOfCall(call))
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeForget(_ context.Context, call *spv2.Call) error {
	out, err := s.forget(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativePublishSession(_ context.Context, call *spv2.Call) error {
	out, err := s.publishSession(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeSharedSessions(_ context.Context, call *spv2.Call) error {
	out, err := s.sharedSessions(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeSharedForget(_ context.Context, call *spv2.Call) error {
	out, err := s.sharedForget(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeConversationOpen(_ context.Context, call *spv2.Call) error {
	out, err := s.conversationOpen(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeConversationArchive(_ context.Context, call *spv2.Call) error {
	out, err := s.conversationArchive(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeTaskCurrent(_ context.Context, call *spv2.Call) error {
	out, err := s.taskCurrent(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeTaskOpen(_ context.Context, call *spv2.Call) error {
	out, err := s.taskOpen(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeTaskFinish(_ context.Context, call *spv2.Call) error {
	out, err := s.taskFinish(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeTaskResume(_ context.Context, call *spv2.Call) error {
	out, err := s.taskResume(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeTaskRename(_ context.Context, call *spv2.Call) error {
	out, err := s.taskRename(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeTaskList(_ context.Context, call *spv2.Call) error {
	out, err := s.taskList(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

// ---------- JSON Schema 小工具 ----------

func objectSchema(props map[string]any, required ...any) map[string]any {
	out := map[string]any{"type": "object"}
	if len(props) > 0 {
		out["properties"] = props
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

func stringSchema() map[string]any  { return map[string]any{"type": "string"} }
func integerSchema() map[string]any { return map[string]any{"type": "integer"} }
func booleanSchema() map[string]any { return map[string]any{"type": "boolean"} }
func stringArraySchema() map[string]any {
	return map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
}
