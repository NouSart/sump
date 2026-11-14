package server

// 租约与 work 状态的生产接线（spec §6.4/§6.3）：
// - 每次真正执行远端工具前，向 memory 的 lease_charge 扣一次 tool_calls；
// - 轮次边界把 work 状态推进（done/cancelled/waiting）。
// 扣不到租约就不执行工具：不做"先跑再说"的静默降级。

import (
	"context"
	"errors"
	"fmt"

	"github.com/NouSart/sump/reasoner/llm"
)

// leaseChargeHook 仅供同包测试注入（nil = 真调用）。
func (s *Server) chargeToolLease(ctx context.Context, trace string) error {
	if s.leaseChargeHook != nil {
		return s.leaseChargeHook(ctx, trace)
	}
	if trace == "" {
		return errors.New("工具执行缺少 trace，无法扣租约")
	}
	_, err := s.Call(ctx, s.memoryName(), "lease_charge", map[string]any{
		"work":  trace,
		"delta": map[string]any{"tool_calls": 1},
	}, trace)
	if err != nil {
		return fmt.Errorf("工具租约扣减失败（不执行本次工具）: %w", err)
	}
	return nil
}

// chargeTokenLease 在每次 LLM 调用成功后按 usage 扣 token 租约（spec §6.4）。
// usage 为 0 的 provider 不扣（不能凭空记账），但也不静默伪造一个值。
func (s *Server) chargeTokenLease(ctx context.Context, trace string, usage llm.Usage) error {
	tokens := usage.TotalTokens
	if tokens == 0 {
		tokens = usage.PromptTokens + usage.CompletionTokens
	}
	if tokens <= 0 {
		return nil
	}
	if trace == "" {
		return errors.New("LLM 调用缺少 trace，无法扣 token 租约")
	}
	if s.tokenChargeHook != nil {
		return s.tokenChargeHook(ctx, trace, tokens)
	}
	_, err := s.Call(ctx, s.memoryName(), "lease_charge", map[string]any{
		"work":  trace,
		"delta": map[string]any{"tokens": tokens},
	}, trace)
	if err != nil {
		return fmt.Errorf("token 租约扣减失败（本轮中止）: %w", err)
	}
	return nil
}

// chargeBeforeTool 是"执行工具前必须先扣租约"的唯一入口；抽出来让同包测试能守住这条判断。
func (s *Server) chargeBeforeTool(ctx context.Context, trace string) error {
	if err := s.chargeToolLease(ctx, trace); err != nil {
		return err
	}
	return nil
}

// updateWorkStatus 在轮次边界推进 work 状态；异步发出，失败会经 Fire 上报设置中心。
func (s *Server) updateWorkStatus(trace, status string) {
	if s.workStatusHook != nil {
		s.workStatusHook(trace, status)
		return
	}
	if trace == "" {
		s.logger.Printf("work 状态推进缺少 trace（status=%s），跳过", status)
		return
	}
	s.Fire(s.memoryName(), "work_update", map[string]any{"work": trace, "status": status}, trace)
}
