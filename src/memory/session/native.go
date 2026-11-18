package session

// SPv2.1 原生入口：/spv2 的 call 直接解 body。
//
// 每个 handler 只做三件事：把 call.Frame.Body 交给共享实现、把帧元信息翻成 callMeta、
// 用 call.Reply / call.ReplyError 回应。业务语义全在共享实现里。
//
// 注册分两档：对外 method（provides 里声明的 12 个）与内部 method（work / A2A /
// context_base / staged_* / settings_schema，只注册、不进 provides）。

import (
	"context"

	"github.com/NouSart/sump/spv2"
)

// registerNative 注册全部动作的原生 method（New 里调用）。
func (s *Server) registerNative() {
	// ---------- 对外 method ----------
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

	// ---------- 内部 method：work 账本（spec §6.8） ----------
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

	// ---------- 内部 method：work 生命周期与 A2A 队列（spec §6.8/§7.4/§7.5） ----------
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

	// ---------- 内部 method：上下文原材料 / 整理池 / 设置声明 ----------
	s.HandleNative(spv2.Method{
		Name:        "context_base",
		Description: "会话侧上下文原材料（内部：任务 / work / 最近记录）",
		Input:       objectSchema(map[string]any{"conversation": stringSchema(), "messages": map[string]any{"type": "array"}}),
		Output:      objectSchema(nil),
	}, s.nativeContextBase)
	s.HandleNative(spv2.Method{
		Name:        "staged_pending",
		Description: "取一批待整理观察（内部）",
		Input:       objectSchema(map[string]any{"limit": integerSchema()}),
		Output:      objectSchema(nil),
	}, s.nativeStagedPending)
	s.HandleNative(spv2.Method{
		Name:        "staged_mark",
		Description: "标记整理池状态（内部）",
		Input:       objectSchema(map[string]any{"id": integerSchema(), "status": stringSchema(), "last_error": stringSchema()}),
		Output:      objectSchema(nil),
	}, s.nativeStagedMark)
	s.HandleNative(spv2.Method{
		Name:        "staged_retry",
		Description: "重试整理池条目（内部）",
		Input:       objectSchema(map[string]any{"id": integerSchema(), "last_error": stringSchema()}),
		Output:      objectSchema(nil),
	}, s.nativeStagedRetry)
	s.HandleNative(spv2.Method{
		Name:        "settings_schema",
		Description: "本进程负责的设置项声明（内部：门面聚合与冲突检查）",
		Input:       objectSchema(nil),
		Output:      objectSchema(nil),
	}, func(_ context.Context, call *spv2.Call) error {
		return call.Reply(s.Settings())
	})
}

// ---------- 原生 handler ----------

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
