package experience

// SPv2.1 原生入口：/spv2 的 call 直接解 body。
//
// 注册分两档：对外 method（provides 里声明的 10 个）与内部 method
// （inject / settings_schema，只注册、不进 provides）。
// Tool 与 provides 里的 Tool 对齐：只有 dig / remember / shared_sessions 是工具。

import (
	"context"

	"github.com/NouSart/sump/spv2"
)

// registerNative 注册全部动作的原生 method（New 里调用）。
func (s *Server) registerNative() {
	// ---------- 对外 method ----------
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
		Name:        "consolidate",
		Description: "处理一批整理池候选（reasoner 分类 + 冲突判断；不暴露给模型）",
		Input:       objectSchema(nil),
		Output:      objectSchema(nil),
	}, s.nativeConsolidate)
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

	// ---------- 内部 method：只在域内可见，不进 provides ----------
	s.HandleNative(spv2.Method{
		Name:        "inject",
		Description: "共享经验四节注入（内部：核心 / 相关 / 线头 / 其他会话）",
		Input:       objectSchema(map[string]any{"viewer": stringSchema(), "origin": stringSchema(), "query": stringSchema()}),
		Output:      objectSchema(nil),
	}, s.nativeInject)
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

// metaOfCall 把 SPv2 帧翻成动作需要的调用元信息。
func metaOfCall(call *spv2.Call) callMeta {
	return callMeta{from: call.Frame.From.ID, trace: call.Frame.Work}
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

func (s *Server) nativeConsolidate(ctx context.Context, call *spv2.Call) error {
	out, err := s.consolidate(ctx, metaOfCall(call))
	if err != nil {
		return call.ReplyError(spv2.CodeInternal, err.Error(), false, nil)
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
