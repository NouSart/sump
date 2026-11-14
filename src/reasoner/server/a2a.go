package server

// reasoner 侧 A2A 最小闭环（spec §6.8）：agent 消息带目标 work 时经 memory 队列入队，
// 该 work 下一次开任务时 claim/ack 取回并注入上下文——跨 work 消息不丢、也不假成功。

import (
	"context"
	"fmt"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

// a2aHooks 是同包测试注入点（生产为 nil）。
type a2aHooks struct {
	post  func(ctx context.Context, work, text, trace string) error
	claim func(ctx context.Context, work string) (protocol.A2AClaimResult, error)
	ack   func(ctx context.Context, id string) error
}

// postA2AMessage 把 agent 消息入队到目标 work。
func (s *Server) postA2AMessage(ctx context.Context, work, text, trace string) error {
	if work == "" {
		return fmt.Errorf("A2A 消息缺少目标 work")
	}
	if s.a2a != nil && s.a2a.post != nil {
		return s.a2a.post(ctx, work, text, trace)
	}
	_, err := s.Call(ctx, s.memoryName(), "message_post", map[string]any{
		"work":   work,
		"to":     map[string]any{"kind": "agent", "id": s.cfg.Name},
		"method": "message",
		"body":   text,
	}, trace)
	if err != nil {
		return fmt.Errorf("A2A 入队失败（不假装已送达）: %w", err)
	}
	return nil
}

// claimA2AMessages 领取目标 work 上排队给本 agent 的消息，并逐条 ack。
// 队列空（not_found）是正常终止；其它错误上抛，由调用方记录。
func (s *Server) claimA2AMessages(ctx context.Context, work string) ([]llm.Message, error) {
	if work == "" {
		return nil, nil
	}
	var out []llm.Message
	for i := 0; i < 32; i += 1 {
		var res protocol.A2AClaimResult
		if s.a2a != nil && s.a2a.claim != nil {
			r, err := s.a2a.claim(ctx, work)
			if err != nil {
				if spv2.IsCode(err, spv2.CodeNotFound) {
					break
				}
				return out, err
			}
			res = r
		} else {
			raw, err := s.Call(ctx, s.memoryName(), "message_claim", map[string]any{"work": work}, work)
			if err != nil {
				if spv2.IsCode(err, spv2.CodeNotFound) {
					break
				}
				return out, err
			}
			if err := protocol.DecodeRaw(raw, &res); err != nil {
				return out, err
			}
		}
		if res.Message.ID == "" {
			break
		}
		label := res.Message.From.Kind + ":" + res.Message.From.ID
		out = append(out, llm.Message{Role: "user", Content: "（A2A · " + label + "）" + res.Message.Body})
		if s.a2a != nil && s.a2a.ack != nil {
			if err := s.a2a.ack(ctx, res.Message.ID); err != nil {
				return out, err
			}
		} else if _, err := s.Call(ctx, s.memoryName(), "message_ack", map[string]any{"id": res.Message.ID}, work); err != nil {
			return out, err
		}
	}
	return out, nil
}
