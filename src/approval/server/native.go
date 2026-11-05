// 原生 SPv2.1 method：approval_request / approval_decide / approval_list / capabilities。
//
// 四个 method 与同名 v1 action 共用业务函数；/spv2 路径直接读 call.Frame.Body，
// 不合成 protocol.JumpPayload，也不合成 protocol.Envelope。
package server

import (
	"context"
	"errors"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// registerNativeMethods 注册 /spv2 的原生 method。
//
// Tool 与 provides 对齐：四条都是服务间 / 管理动作，均不暴露给模型。
// approval_request 由 shell 同步等待裁决（ReviewTTL 内），不是「受理即回执」，
// 所以也不声明 LongRunning。
func (s *Server) registerNativeMethods() {
	s.HandleNative(spv2.Method{
		Name:        protocol.ApprovalAction,
		Description: "命令审批：返回终端裁决与 LLM 反馈（白名单直过，其余挂起等门户裁决）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"schema_version": map[string]any{"type": "integer"},
				"item_id":        map[string]any{"type": "string"},
				"plan": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"version":          map[string]any{"type": "integer"},
						"command":          map[string]any{"type": "string"},
						"work_dir":         map[string]any{"type": "string"},
						"shell_path":       map[string]any{"type": "string"},
						"args":             map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"dialect":          map[string]any{"type": "string"},
						"platform":         map[string]any{"type": "string"},
						"profile":          map[string]any{"type": "string"},
						"timeout_ms":       map[string]any{"type": "integer"},
						"max_output_bytes": map[string]any{"type": "integer"},
					},
					"required": []any{"command", "work_dir"},
				},
			},
			"required": []any{"item_id", "plan"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"schema_version": map[string]any{"type": "integer"},
				"item_id":        map[string]any{"type": "string"},
				"decision_id":    map[string]any{"type": "string"},
				"decision":       map[string]any{"type": "string", "enum": []any{protocol.DecisionAllow, protocol.DecisionDeny, protocol.DecisionReview}},
				"plan_digest":    map[string]any{"type": "string"},
				"policy_version": map[string]any{"type": "string"},
				"reason_code":    map[string]any{"type": "string"},
				"valid_for_ms":   map[string]any{"type": "integer"},
				"llm_feedback": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"summary":  map[string]any{"type": "string"},
						"unknowns": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
						"rule_ids": map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
					},
				},
			},
		},
	}, s.nativeApprove)
	s.HandleNative(spv2.Method{
		Name:        protocol.ApprovalDecideAction,
		Description: "门户裁决一条待审命令（仅门户可调）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id":  map[string]any{"type": "string"},
				"decision": map[string]any{"type": "string", "enum": []any{protocol.DecisionAllow, protocol.DecisionDeny}},
				"note":     map[string]any{"type": "string"},
			},
			"required": []any{"item_id", "decision"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"status":   map[string]any{"type": "string", "enum": []any{"accepted", "already_decided"}},
				"item_id":  map[string]any{"type": "string"},
				"decision": map[string]any{"type": "string"},
			},
			"required": []any{"status", "item_id"},
		},
	}, s.nativeDecide)
	s.HandleNative(spv2.Method{
		Name:        protocol.ApprovalListAction,
		Description: "列出门户当前的待审命令（仅门户可调）",
		Input:       map[string]any{"type": "object"},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"items": map[string]any{
					"type": "array",
					"items": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"item_id":      map[string]any{"type": "string"},
							"command":      map[string]any{"type": "string"},
							"work_dir":     map[string]any{"type": "string"},
							"requested_by": map[string]any{"type": "string"},
							"reason_code":  map[string]any{"type": "string"},
							"summary":      map[string]any{"type": "string"},
							"unknowns":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"rule_ids":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
							"created_ms":   map[string]any{"type": "integer"},
							"expires_ms":   map[string]any{"type": "integer"},
						},
					},
				},
			},
		},
	}, s.nativeList)
	s.HandleNative(spv2.Method{
		Name:        "capabilities",
		Description: "审批服务的方言、第二层状态与档位（管理/调试动作，不暴露给模型）",
		Input:       map[string]any{"type": "object"},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"schema_version": map[string]any{"type": "integer"},
				"dialects":       map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
				"profile":        map[string]any{"type": "string"},
				"other_dialects": map[string]any{"type": "string"},
				"feedback_route": map[string]any{"type": "string"},
				"review_enabled": map[string]any{"type": "boolean"},
				"review_portal":  map[string]any{"type": "string"},
				"review_ttl_ms":  map[string]any{"type": "integer"},
				"auto_approve":   map[string]any{"type": "string"},
				"pending":        map[string]any{"type": "integer"},
				"accepted":       map[string]any{"type": "integer"},
				"busy":           map[string]any{"type": "integer"},
			},
		},
	}, s.nativeCapabilities)
}

// nativeApprove 是 /spv2 的原生入口：直接解 body，不经过 v1 JumpPayload。
func (s *Server) nativeApprove(ctx context.Context, call *spv2.Call) error {
	// SPv2 帧里没有 boss 字段；沿用调用方 id，与 timer 的原生入口同口径。
	out, err := s.approve(ctx, call.Frame.From.ID, call.Frame.Work, call.Frame.From.ID, call.Frame.Body)
	if err != nil {
		return call.ReplyError(nativeErrorCode(err), err.Error(), false, nil)
	}
	return call.Reply(out)
}

// nativeDecide 是 /spv2 的原生入口。
func (s *Server) nativeDecide(_ context.Context, call *spv2.Call) error {
	out, err := s.decide(call.Frame.From.ID, call.Frame.Body)
	if err != nil {
		return call.ReplyError(nativeErrorCode(err), err.Error(), false, nil)
	}
	return call.Reply(out)
}

// nativeList 是 /spv2 的原生入口。
func (s *Server) nativeList(_ context.Context, call *spv2.Call) error {
	out, err := s.list(call.Frame.From.ID)
	if err != nil {
		return call.ReplyError(nativeErrorCode(err), err.Error(), false, nil)
	}
	return call.Reply(out)
}

// nativeCapabilities 是 /spv2 的原生入口。
func (s *Server) nativeCapabilities(_ context.Context, call *spv2.Call) error {
	return call.Reply(s.capabilities())
}

// nativeErrorCode 把共享实现的错误映射成 SPv2 错误码。
// 其它错误（参数无效、越权、待审项不存在、服务繁忙等）统一按 invalid_argument 回；
// 调用方放弃等待与超时则如实回报，便于上层区分「请求不对」和「没等到」。
func nativeErrorCode(err error) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return spv2.CodeDeadlineExceeded
	case errors.Is(err, context.Canceled):
		return spv2.CodeCancelled
	default:
		return spv2.CodeInvalidArgument
	}
}
