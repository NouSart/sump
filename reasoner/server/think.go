package server

// 单步推理（think）：所有非任务型模型调用的统一出口。
//
// 为什么要有它：复盘写经验、给经验簇起短标签这类活，需要的只是"给一段提示词、
// 要一段文本"，不该为此发明一个新任务（那要过黑板、过收尾门禁、还会在历史里
// 留下一条假对话），更不该让每个服务自己配一份 LLM Key（密钥散落 = 谁也说不清
// 钱是谁花的）。所以出口只有一个：jump 到 llm 服务的 think。
//
// 它与任务链**没有交集**：不带工具、不进黑板、不写记忆、不唤醒任何轮次。
// 失败就如实返回错误——调用方自己决定是重试、降级还是算了。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

const (
	// thinkMaxPromptChars 是提示词硬上限：一个误拼的循环能把账单打飞，
	// 所以宁可在这里明确拒绝，也不要把请求发出去再后悔。
	thinkMaxPromptChars = 200_000
	// thinkMaxTokens 是输出上限：think 只生产"一段话"，不需要长篇。
	thinkMaxTokens = 4096
	// thinkTimeout 是单次补全的墙钟上限。Call 本身有超时，这里再兜一层，
	// 免得一个卡住的上游把调用方的连接也拖住。
	thinkTimeout = 120 * time.Second
)

// actionThink 跑一次单步推理，返回模型的纯文本。
// 不进 provides：LLM 不该有"让自己再想一次"的按钮（那是无限自我循环的起点）。
func (s *Server) actionThink(ctx context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ThinkPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	prompt := strings.TrimSpace(p.Prompt)
	if prompt == "" {
		return nil, errors.New("think: prompt 不能为空")
	}
	if len(prompt)+len(p.System) > thinkMaxPromptChars {
		return nil, fmt.Errorf("think: 提示词太长（%d 字符 > 上限 %d）", len(prompt)+len(p.System), thinkMaxPromptChars)
	}
	maxTokens := p.MaxTokens
	if maxTokens <= 0 || maxTokens > thinkMaxTokens {
		maxTokens = thinkMaxTokens
	}

	client := s.llmClient()
	if client == nil {
		return nil, errors.New("think: LLM 未配置")
	}

	msgs := make([]llm.Message, 0, 2)
	if sys := strings.TrimSpace(p.System); sys != "" {
		msgs = append(msgs, llm.Message{Role: "system", Content: sys})
	}
	msgs = append(msgs, llm.Message{Role: "user", Content: p.Prompt})

	cctx, cancel := context.WithTimeout(ctx, thinkTimeout)
	defer cancel()
	resp, err := client.Chat(cctx, llm.ChatRequest{
		Model:     strings.TrimSpace(p.Model),
		Messages:  msgs,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return nil, fmt.Errorf("think: %w", err)
	}
	return protocol.ThinkResult{Text: resp.Content}, nil
}
