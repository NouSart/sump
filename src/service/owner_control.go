package service

// owner 侧的对账 / 控制面（R-029）：把 owner_status / owner_control 两个 native method
// 接到服务自己的实现上。载荷与取值见 protocol.Owner*Payload 与
// docs/concepts/protocol.md §5.4——这两个 method 是 memory call_status / call_control 的
// 执行服务对端，不是工具，也不进 provides。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// OwnerControl 是服务侧的账本对账 / 控制实现（可选）。
//
// 未设置时两个 method 不注册：direct call 得 not_found。不提供「默认成功」——owner 服务
// 若没有真实的状态账本，宁可让 method 不存在，也不能回一个编出来的状态（S-001 INV-4）。
type OwnerControl interface {
	// OwnerStatus 按 id 批量回报执行维（protocol 的 exec 词汇）。
	// states 只包含本服务认识的 id；recoveryComplete=false 表示启动早期的持久受理记录
	// 尚未恢复完——此时「不认识」不能判 orphaned（S-001 §3.5 规则 1）。
	OwnerStatus(ctx context.Context, ids []string) (states map[string]string, recoveryComplete bool, err error)
	// OwnerControl 执行一次控制：op 取 protocol.OwnerOp*；ttl 是调用方给的等待上限
	// （0 = 没给，只受服务 action timeout 约束）。
	// exec 是 owner 确认后的执行维：成功应为 protocol.OwnerExec* 之一；空串表示已确认但
	// 未带状态维；其它值视为未确认，reason 说明原因。失败必须回错误（带 spv2 错误码），
	// 不得用结果字段假装成功。
	OwnerControl(ctx context.Context, id, op string, ttl time.Duration) (exec, reason string, err error)
}

// SetOwnerControl 接入控制面并注册 owner_status / owner_control。
//
// 必须在 Start 之前调用：服务卡在 Start 时定稿，之后再注册会造出「卡里没有、却能被直接
// 调到」的不一致。两个 method 不是工具、不进 provides。
func (s *Service) SetOwnerControl(h OwnerControl) {
	if h == nil {
		return
	}
	s.mu.Lock()
	started := s.started
	if !started {
		s.ownerControl = h
	}
	s.mu.Unlock()
	if started {
		s.logger.Printf("%s: 忽略 Start 之后接入的 owner 控制面（owner_status / owner_control 未注册）", s.cfg.Name)
		return
	}
	s.HandleNative(spv2.Method{
		Name:        "owner_status",
		Description: "owner 侧对账：按受理句柄批量回报执行维（memory call_status 的服务侧对端）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"ids": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "待对账的受理句柄（本服务发过的 id）",
				},
			},
			"required": []any{"ids"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"recovery_complete": map[string]any{
					"type":        "boolean",
					"description": "持久受理记录是否已恢复完整；false 时「不认识」不能判 orphaned",
				},
				"states": map[string]any{
					"type":                 "object",
					"additionalProperties": map[string]any{"type": "string"},
					"description":          "call_id → 执行维；只含本服务认识的 id",
				},
			},
			"required": []any{"recovery_complete", "states"},
		},
	}, s.nativeOwnerStatus)
	s.HandleNative(spv2.Method{
		Name:        "owner_control",
		Description: "owner 侧控制：对一次已受理的执行做 pause / resume / cancel（memory call_control 的服务侧对端）",
		Input: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string", "description": "受理句柄（本服务发过的 id）"},
				"op": map[string]any{"type": "string", "enum": []any{
					protocol.OwnerOpPause, protocol.OwnerOpResume, protocol.OwnerOpCancel,
				}},
				"ttl_ms": map[string]any{
					"type": "integer", "minimum": 0,
					"description": "本轮控制等待确认的上限（毫秒）；缺省只受服务 action timeout 约束",
				},
			},
			"required": []any{"id", "op"},
		},
		Output: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string"},
				"exec": map[string]any{
					"type":        "string",
					"description": "确认后的执行维：paused / running / cancelled；空串 = 已确认但未带状态维；其它值 = 未确认",
				},
				"reason": map[string]any{"type": "string", "description": "未确认或部分生效的原因"},
			},
			"required": []any{"id", "exec"},
		},
	}, s.nativeOwnerControl)
}

// nativeOwnerStatus 处理 owner_status：解码 ids → 问 hook → 回 states + recovery_complete。
func (s *Service) nativeOwnerStatus(ctx context.Context, call *spv2.Call) error {
	scope := s.cfg.Name + ".owner_status"
	var p protocol.OwnerStatusPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, scope+": 载荷解析失败: "+err.Error(), false, nil)
	}
	if len(p.IDs) == 0 {
		return spv2.ErrCode(spv2.CodeInvalidArgument, scope+": ids 不能为空", false, nil)
	}
	h := s.ownerControlHook()
	if h == nil {
		// 正常路径不可能到这里（method 只在设置 hook 后注册）；防御「注册后又被清空」。
		return spv2.ErrCode(spv2.CodeUnavailable, scope+": 未接入控制面", false, nil)
	}
	states, complete, err := h.OwnerStatus(ctx, p.IDs)
	if err != nil {
		return annotateOwnerErr(scope, err)
	}
	if states == nil {
		states = map[string]string{}
	}
	return call.Reply(protocol.OwnerStatusResult{RecoveryComplete: complete, States: states})
}

// nativeOwnerControl 处理 owner_control：校验 op / ttl → 用 ttl 约束 hook 的 ctx → 回确认后的执行维。
func (s *Service) nativeOwnerControl(ctx context.Context, call *spv2.Call) error {
	var p protocol.OwnerControlPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument,
			s.cfg.Name+".owner_control: 载荷解析失败: "+err.Error(), false, nil)
	}
	scope := fmt.Sprintf("%s.owner_control id=%q op=%q", s.cfg.Name, p.ID, p.Op)
	if p.ID == "" {
		return spv2.ErrCode(spv2.CodeInvalidArgument, s.cfg.Name+".owner_control: id 必填", false, nil)
	}
	switch p.Op {
	case protocol.OwnerOpPause, protocol.OwnerOpResume, protocol.OwnerOpCancel:
	default:
		return spv2.ErrCode(spv2.CodeInvalidArgument,
			scope+": op 只能是 pause / resume / cancel", false, nil)
	}
	if p.TTLMS < 0 {
		return spv2.ErrCode(spv2.CodeInvalidArgument, scope+": ttl_ms 不能为负", false, nil)
	}
	h := s.ownerControlHook()
	if h == nil {
		return spv2.ErrCode(spv2.CodeUnavailable, scope+": 未接入控制面", false, nil)
	}
	ttl := time.Duration(p.TTLMS) * time.Millisecond
	hctx := ctx
	if ttl > 0 {
		// 调用方给的 ttl 是对账/控制握手的等待上限：用 ctx 传下去，hook 内不再自设定时器。
		var cancel context.CancelFunc
		hctx, cancel = context.WithTimeout(ctx, ttl)
		defer cancel()
	}
	exec, reason, err := h.OwnerControl(hctx, p.ID, p.Op, ttl)
	if err != nil {
		return annotateOwnerErr(scope, err)
	}
	return call.Reply(protocol.OwnerControlResult{ID: p.ID, Exec: exec, Reason: reason})
}

// ownerControlHook 取已接入的实现（Start 前写入；handler 可能在别的 goroutine）。
func (s *Service) ownerControlHook() OwnerControl {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ownerControl
}

// annotateOwnerErr 给 hook 的错误补上「哪个服务、哪个对象、哪个动作」，并**保留错误码**：
// 调用方按码归类（not_supported / not_found / deadline_exceeded …），加了前缀不能退化成 internal。
func annotateOwnerErr(scope string, err error) error {
	if e, ok := err.(*spv2.Error); ok {
		return spv2.ErrCode(e.Code, scope+": "+e.Message, e.Retryable, e.Details)
	}
	return fmt.Errorf("%s: %w", scope, err)
}
