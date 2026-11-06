package server

// 原生 SPv2.1 method。这里只做三件事：
//   - 声明 method（名字 / Tool / Input / Output JSON Schema，工具标志与 Provides 对齐）；
//   - 直接解 call.Frame.Body，调共享实现；
//   - 用 call.Reply / call.ReplyError 回应。
//
// 信封里曾经的元信息改从帧头取：from = Frame.From.ID、trace = Frame.Work、
// boss = Frame.From.ID（SPv2 没有 boss 字段，from 就是结果的归属方）。

import (
	"context"
	"errors"

	"github.com/NouSart/sump/spv2"
)

// permissionError 是来源校验失败的领域错误：v1 只透出错误文本，
// 原生入口据此回 permission_denied（而不是把“没权限”说成“参数不对”）。
type permissionError struct{ msg string }

func (e *permissionError) Error() string { return e.msg }

// replyNativeError 把共享实现的错误映射成 SPv2 错误码并回应。
func replyNativeError(call *spv2.Call, err error) error {
	code := spv2.CodeInvalidArgument
	var perm *permissionError
	if errors.As(err, &perm) {
		code = spv2.CodePermissionDenied
	}
	return call.ReplyError(code, err.Error(), false, nil)
}

// registerNativeMethods 注册 9 个原生 method。
// Tool 一律为 false：reasoner 的动作是服务间链路的控制面，Provides 里没有 tool:true，
// 模型可见的工具来自名册里的其它服务。9 个动作也都不是长耗时受理（没有 Async）。
func (s *Server) registerNativeMethods() {
	s.HandleNative(spv2.Method{
		Name:        "echo",
		Description: "回显：原样返回收到的输入（测试 / 调试用）",
		Input:       map[string]any{"type": "object", "description": "任意值，原样返回"},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"who":  map[string]any{"type": "string"},
				"echo": map[string]any{"description": "原样返回的输入"},
			},
		},
	}, s.nativeEcho)
	s.HandleNative(spv2.Method{
		Name:        "debug_jump",
		Description: "调试：替调用方向目标服务发起一次跳转（to=self 或服务名）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"to":     map[string]any{"type": "string"},
				"action": map[string]any{"type": "string"},
				"input":  map[string]any{"description": "目标动作的载荷"},
			},
			"required": []any{"to", "action"},
		},
		Output: map[string]any{"type": "object", "description": "目标服务的响应数据"},
	}, s.nativeDebugJump)
	s.HandleNative(spv2.Method{
		Name:        "think",
		Description: "单步推理：一次补全，不带工具、不开任务、不进黑板",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"system":     map[string]any{"type": "string"},
				"prompt":     map[string]any{"type": "string"},
				"model":      map[string]any{"type": "string"},
				"max_tokens": map[string]any{"type": "integer"},
			},
			"required": []any{"prompt"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text": map[string]any{"type": "string"},
			},
		},
	}, s.nativeThink)
	s.HandleNative(spv2.Method{
		Name:        "user_message",
		Description: "链入口：用户消息先托管给记忆服务，收到 resume 才完全启动",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"text":            map[string]any{"type": "string"},
				"conversation_id": map[string]any{"type": "string"},
				"images":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"files":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"sender":          map[string]any{"type": "object", "description": "来源标注（谁 / 群聊 / 扩散面）"},
				"notes":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			// 与 v1 校验一致：text 与 images 至少一个。
			"anyOf": []any{
				map[string]any{"required": []any{"text"}},
				map[string]any{"required": []any{"images"}},
			},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
			},
		},
	}, s.nativeUserMessage)
	s.HandleNative(spv2.Method{
		Name:        "step",
		Description: "自跳：把黑板上的任务推进到下一轮",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"conversation_id": map[string]any{"type": "string"},
			},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string", "description": "accepted / ignored"},
			},
		},
	}, s.nativeStep)
	s.HandleNative(spv2.Method{
		Name:        "resume",
		Description: "完全启动：仅接受记忆服务的 resume，开启任务并推进推理",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"context": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"conversation_id": map[string]any{"type": "string"},
						"messages":        map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
						"boss":            map[string]any{"type": "string"},
						"task":            map[string]any{"type": "object"},
						"task_name":       map[string]any{"type": "string"},
					},
					"required": []any{"messages"},
				},
			},
			"required": []any{"context"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
			},
		},
	}, s.nativeResume)
	s.HandleNative(spv2.Method{
		Name:        "report_result",
		Description: "补报：异步动作的后续结果，按来源 / trace / item_id 关联",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string"},
				"phase":   map[string]any{"type": "string"},
				"text":    map[string]any{"type": "string"},
				"data":    map[string]any{"description": "服务自定的结构化补充"},
				"final":   map[string]any{"type": "boolean"},
			},
			"required": []any{"item_id", "phase"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":   map[string]any{"type": "string", "description": "accepted / buffered / ignored / duplicate"},
				"delivery": map[string]any{"type": "string"},
			},
		},
	}, s.nativeReportResult)
	s.HandleNative(spv2.Method{
		Name:        "user_hold",
		Description: "用户停手：入口服务把界面上的暂停 / 停止告诉推理侧",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"conversation_id": map[string]any{"type": "string"},
				"hold":            map[string]any{"type": "string", "enum": []any{"none", "secondary", "primary"}},
				"paused_items":    map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"notes":           map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []any{"conversation_id"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status": map[string]any{"type": "string"},
				"level":  map[string]any{"type": "string"},
			},
		},
	}, s.nativeUserHold)
	s.HandleNative(spv2.Method{
		Name:        "retry",
		Description: "重试：门户对上一轮失败、任务还开着的会话再跑一轮",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"conversation_id": map[string]any{"type": "string"},
			},
			"required": []any{"conversation_id"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"retried":         map[string]any{"type": "boolean"},
				"conversation_id": map[string]any{"type": "string"},
			},
		},
	}, s.nativeRetry)
}

// ---------- 原生入口（直接解 body，不经过 v1 跳转载荷） ----------

func (s *Server) nativeEcho(_ context.Context, call *spv2.Call) error {
	out, err := s.echo(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeDebugJump(ctx context.Context, call *spv2.Call) error {
	out, err := s.debugJump(ctx, call.Frame.Work, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeThink(ctx context.Context, call *spv2.Call) error {
	out, err := s.think(ctx, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeUserMessage(_ context.Context, call *spv2.Call) error {
	// SPv2 没有 boss：from 既是入口，也是结果的归属方。
	out, err := s.userMessage(call.Frame.From.ID, call.Frame.Work, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeStep(_ context.Context, call *spv2.Call) error {
	out, err := s.step(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeResume(_ context.Context, call *spv2.Call) error {
	// SPv2 没有 boss：缺省结果归属方取 from；任务原 boss 随 context 回来时优先。
	out, err := s.resume(call.Frame.From.ID, call.Frame.From.ID, call.Frame.Work, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeReportResult(_ context.Context, call *spv2.Call) error {
	out, err := s.reportResult(call.Frame.From.ID, call.Frame.To.ID, call.Frame.Work, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeUserHold(_ context.Context, call *spv2.Call) error {
	out, err := s.userHold(call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeRetry(_ context.Context, call *spv2.Call) error {
	out, err := s.retry(call.Frame.From.ID, call.Frame.Body)
	if err != nil {
		return replyNativeError(call, err)
	}
	return call.Reply(out)
}
