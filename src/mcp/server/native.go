package server

// SPv2.1 原生入口：/spv2 的 call 直接解 Frame.Body 并调用业务函数；
// 服务只暴露原生 method，不构造跳转载荷，也不合成信封。

import (
	"context"
	"errors"
	"strings"

	"github.com/NouSart/sump/spv2"
)

// registerNativeMethods 把每个动态 provide 注册为原生 SPv2.1 method，
// 并为 report_result / cancel_import 注册原生入口。服务只暴露 /spv2，
// 不再注册同名 v1 action。
func (s *Server) registerNativeMethods() {
	for _, method := range s.imports.NativeMethods() {
		s.svc.HandleNative(method, s.nativeImport(method.Name))
	}
	s.svc.HandleNative(spv2.Method{
		Name:        "report_result",
		Description: "接收异步调用的补报（调用方按 trace 关联）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"item_id": map[string]any{"type": "string"},
				"phase":   map[string]any{"type": "string"},
				"text":    map[string]any{"type": "string"},
				"data":    map[string]any{},
				"final":   map[string]any{"type": "boolean"},
			},
			"required": []any{"item_id", "phase"},
		},
		Output: map[string]any{"type": "object"},
	}, s.nativeReport)
	s.svc.HandleNative(spv2.Method{
		Name:        "cancel_import",
		Description: "撤销本进程内尚未完成的异步 import 观察",
		Input: map[string]any{
			"type":       "object",
			"properties": map[string]any{"item_id": map[string]any{"type": "string"}},
			"required":   []any{"item_id"},
		},
		Output: map[string]any{"type": "object"},
	}, s.nativeCancelImport)
}

// nativeImport 返回某个动态 provide 的原生 handler；method 已在注册时绑定。
func (s *Server) nativeImport(action string) func(context.Context, *spv2.Call) error {
	return func(ctx context.Context, call *spv2.Call) error {
		out, err := s.importCall(ctx, action, call.Frame.Body, call.Frame.From.ID, call.Frame.Work)
		if err != nil {
			code, retryable := nativeImportError(err)
			return call.ReplyError(code, err.Error(), retryable, nil)
		}
		return call.Reply(out)
	}
}

func (s *Server) nativeReport(_ context.Context, call *spv2.Call) error {
	if call.Frame.To.Kind != "service" || call.Frame.To.ID != "mcp" {
		return call.ReplyError(spv2.CodePermissionDenied, "report target does not match an MCP observation", false, nil)
	}
	out, err := s.reportResult(call.Frame.Work, call.Frame.From.ID, call.Frame.Body)
	if err != nil {
		return call.ReplyError(nativeReportError(err), err.Error(), false, nil)
	}
	return call.Reply(out)
}

func (s *Server) nativeCancelImport(_ context.Context, call *spv2.Call) error {
	out, err := s.cancelImport(call.Frame.Body, call.Frame.From.ID)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}

// nativeImportError 把 import 的错误前缀映射为 SPv2 标准错误码；retryable
// 只在“远端工具可以确认未被提交或明确可重试”时为 true。
func nativeImportError(err error) (string, bool) {
	message := err.Error()
	switch {
	case strings.HasPrefix(message, "MCP_INVALID_INPUT"), strings.HasPrefix(message, "MCP_IMPORT_REJECTED"):
		return spv2.CodeInvalidArgument, false
	case strings.HasPrefix(message, "MCP_BUSY"), strings.Contains(message, "concurrency limit"):
		return spv2.CodeResourceExhausted, true
	case strings.HasPrefix(message, "MCP_AUTH_REQUIRED"), strings.Contains(message, "credential revoked"):
		return spv2.CodePermissionDenied, false
	case strings.HasPrefix(message, "MCP_PROTOCOL_MISMATCH"), strings.HasPrefix(message, "MCP_CONTRACT_MISMATCH"):
		return spv2.CodeNotSupported, false
	case strings.HasPrefix(message, "MCP_REMOTE_UNAVAILABLE"), strings.HasPrefix(message, "MCP_DISCOVERY_FAILED"):
		return spv2.CodeUnavailable, true
	case strings.HasPrefix(message, "MCP_OUTCOME_UNKNOWN"), strings.HasPrefix(message, "MCP_INPUT_REQUIRED"):
		return spv2.CodeUnavailable, false
	case strings.Contains(message, "MCP service closed"):
		return spv2.CodeUnavailable, true
	case errors.Is(err, context.Canceled):
		return spv2.CodeCancelled, false
	case errors.Is(err, context.DeadlineExceeded):
		return spv2.CodeDeadlineExceeded, true
	default:
		return spv2.CodeInternal, false
	}
}

func nativeReportError(err error) string {
	switch {
	case errors.Is(err, ErrJobResultLimit):
		return spv2.CodeResourceExhausted
	case strings.Contains(err.Error(), "invalid report"), strings.Contains(err.Error(), "item_id"):
		return spv2.CodeInvalidArgument
	default:
		return spv2.CodeInternal
	}
}
