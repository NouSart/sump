package server

// batch 的原生 SPv2.1 method：Provides 里的每个动作都注册成一个 method。
// 业务实现在共享函数（run/status/result/list/control）；
// 这里只从 Frame 取调用元数据、调用共享函数并回 reply。

import (
	"context"

	"github.com/NouSart/sump/spv2"
)

// registerNativeMethods 注册全部原生 method（必须在 Start 前调用）。
func (s *Server) registerNativeMethods() {
	s.HandleNative(spv2.Method{
		Name: "run", Tool: true, LongRunning: true,
		Description: "批处理：对目标服务的某个动作批量执行；受理即回执，跑完推一条汇总",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"service": map[string]any{"type": "string", "description": "目标服务名"},
				"action":  map[string]any{"type": "string", "description": "目标动作名（必须声明可批量）"},
				"items": map[string]any{
					"type":        "array",
					"minItems":    1,
					"description": "要执行的项；input 原样透传，batch 不解析、不改写",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"input": map[string]any{"description": "原样透传给目标动作的 input"},
							"key":   map[string]any{"type": "boolean", "description": "关键项：wake.mode=key 时全部结束会提前唤醒"},
							"tag":   map[string]any{"type": "string", "description": "可选标识，汇总与明细里原样带回"},
						},
						"required": []any{"input"},
					},
				},
				"concurrency": map[string]any{"type": "integer", "minimum": 0,
					"description": "单任务并发；0/缺省用服务默认，上限 max_concurrency"},
				"on_error": map[string]any{"type": "string", "enum": []any{"continue", "stop"},
					"description": "continue（默认）单项失败只记该项；stop 首项失败后未开始的项转 skipped"},
				"timeout_ms": map[string]any{"type": "integer", "minimum": 0,
					"description": "单项超时（毫秒）；0/缺省用服务默认"},
				"kind": map[string]any{"type": "string", "enum": []any{"once", "resident"},
					"description": "任务类别；只影响唤醒聚合，不影响执行"},
				"wake": map[string]any{
					"type":        "object",
					"description": "唤醒策略：all（默认）等全部项结束；key 在全部关键项结束时提前唤醒一次",
					"properties": map[string]any{
						"mode": map[string]any{"type": "string", "enum": []any{"all", "key"}},
					},
				},
			},
			"required": []any{"service", "action", "items"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string", "description": "任务句柄（补报、状态、明细、控制都用它）"},
				"total":   map[string]any{"type": "integer"},
				"phase":   map[string]any{"type": "string", "enum": []any{"accepted"}},
				"kind":    map[string]any{"type": "string", "enum": []any{"once", "resident"}},
			},
			"required": []any{"item_id", "total", "phase"},
		},
	}, s.nativeRun)

	s.HandleNative(spv2.Method{
		Name: "status", Tool: true,
		Description: "查单个批量任务的进度快照",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string", "description": "run 回执里的句柄"},
			},
			"required": []any{"item_id"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string"},
				"service": map[string]any{"type": "string"},
				"action":  map[string]any{"type": "string"},
				"kind":    map[string]any{"type": "string"},
				"phase": map[string]any{"type": "string",
					"enum": []any{"accepted", "running", "paused", "done", "failed", "cancelled"}},
				"total":      map[string]any{"type": "integer"},
				"ok":         map[string]any{"type": "integer"},
				"failed":     map[string]any{"type": "integer"},
				"skipped":    map[string]any{"type": "integer"},
				"pending":    map[string]any{"type": "integer", "description": "尚未开始 + 正在跑"},
				"key_total":  map[string]any{"type": "integer"},
				"key_done":   map[string]any{"type": "integer"},
				"woken":      map[string]any{"type": "boolean"},
				"elapsed_ms": map[string]any{"type": "integer"},
			},
			"required": []any{"item_id", "phase", "total", "ok", "failed", "skipped", "pending"},
		},
	}, s.nativeStatus)

	s.HandleNative(spv2.Method{
		Name: "result", Tool: true,
		Description: "分页读取批量任务的明细（只存摘要，完整结果要就再调一次目标动作）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string", "description": "run 回执里的句柄"},
				"offset":  map[string]any{"type": "integer", "minimum": 0},
				"limit":   map[string]any{"type": "integer", "minimum": 0, "description": "默认 20，上限 100"},
				"status": map[string]any{"type": "string", "enum": []any{"ok", "failed", "skipped", "pending"},
					"description": "只看某一类项"},
			},
			"required": []any{"item_id"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string"},
				"offset":  map[string]any{"type": "integer"},
				"limit":   map[string]any{"type": "integer"},
				"matched": map[string]any{"type": "integer", "description": "满足 status 过滤的总条数"},
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"seq":    map[string]any{"type": "integer"},
							"tag":    map[string]any{"type": "string"},
							"key":    map[string]any{"type": "boolean"},
							"status": map[string]any{"type": "string", "enum": []any{"pending", "running", "ok", "failed", "skipped"}},
							"error":  map[string]any{"type": "string"},
							"preview": map[string]any{"type": "string",
								"description": "响应摘要（截断到 preview_bytes）"},
							"size": map[string]any{"type": "integer", "description": "原始响应字节数（截断前）"},
							"ms":   map[string]any{"type": "integer"},
						},
						"required": []any{"seq", "status"},
					},
				},
			},
			"required": []any{"item_id", "offset", "limit", "matched", "items"},
		},
	}, s.nativeResult)

	s.HandleNative(spv2.Method{
		Name: "list", Tool: true,
		Description: "查批量任务队列（模型与前端共用；拉取不唤醒模型）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"phase": map[string]any{"type": "string",
					"enum":        []any{"accepted", "running", "paused", "done", "failed", "cancelled"},
					"description": "按阶段过滤；缺省为全部"},
				"limit":  map[string]any{"type": "integer", "minimum": 0, "description": "默认 20，上限 100"},
				"caller": map[string]any{"type": "string", "description": "只看某个发起方"},
			},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"jobs": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"item_id":    map[string]any{"type": "string"},
							"service":    map[string]any{"type": "string"},
							"action":     map[string]any{"type": "string"},
							"caller":     map[string]any{"type": "string"},
							"kind":       map[string]any{"type": "string"},
							"phase":      map[string]any{"type": "string"},
							"total":      map[string]any{"type": "integer"},
							"ok":         map[string]any{"type": "integer"},
							"failed":     map[string]any{"type": "integer"},
							"skipped":    map[string]any{"type": "integer"},
							"running":    map[string]any{"type": "integer"},
							"pending":    map[string]any{"type": "integer"},
							"key_total":  map[string]any{"type": "integer"},
							"key_done":   map[string]any{"type": "integer"},
							"summary":    map[string]any{"type": "string"},
							"elapsed_ms": map[string]any{"type": "integer"},
						},
						"required": []any{"item_id", "phase", "total"},
					},
				},
				"counts": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"running": map[string]any{"type": "integer"},
						"paused":  map[string]any{"type": "integer"},
						"done":    map[string]any{"type": "integer"},
						"failed":  map[string]any{"type": "integer"},
					},
				},
			},
			"required": []any{"jobs", "counts"},
		},
	}, s.nativeList)

	s.HandleNative(spv2.Method{
		Name: "control", Tool: true,
		Description: "暂停 / 恢复 / 停止批量任务",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":    map[string]any{"type": "string", "enum": []any{"pause", "resume", "cancel"}},
				"scope": map[string]any{"type": "string", "enum": []any{"one", "all"}, "description": "缺省为 one"},
				"item_id": map[string]any{"type": "string",
					"description": "scope=one 时必填"},
			},
			"required": []any{"op"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":       map[string]any{"type": "string"},
				"scope":    map[string]any{"type": "string"},
				"affected": map[string]any{"type": "integer", "description": "实际改变了状态的任务数"},
				"paused":   map[string]any{"type": "integer"},
				"running":  map[string]any{"type": "integer"},
			},
			"required": []any{"op", "scope", "affected", "paused", "running"},
		},
	}, s.nativeControl)
}

// nativeReply 把共享函数的返回值 / 错误统一回成 SPv2 reply。
// 业务错误按参数错误回；错误文本保持原样（BATCH_* 前缀不变）。
func nativeReply(call *spv2.Call, out any, err error) error {
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeRun(_ context.Context, call *spv2.Call) error {
	// SPv2 帧没有独立 boss，沿用调用方作为结果归属；trace 取 work。
	caller := call.Frame.From.ID
	out, err := s.run(caller, caller, call.Frame.Work, call.Frame.Body)
	return nativeReply(call, out, err)
}

func (s *Server) nativeStatus(_ context.Context, call *spv2.Call) error {
	out, err := s.status(call.Frame.Body)
	return nativeReply(call, out, err)
}

func (s *Server) nativeResult(_ context.Context, call *spv2.Call) error {
	out, err := s.result(call.Frame.Body)
	return nativeReply(call, out, err)
}

func (s *Server) nativeList(_ context.Context, call *spv2.Call) error {
	out, err := s.list(call.Frame.Body)
	return nativeReply(call, out, err)
}

func (s *Server) nativeControl(_ context.Context, call *spv2.Call) error {
	out, err := s.control(call.Frame.Body)
	return nativeReply(call, out, err)
}
