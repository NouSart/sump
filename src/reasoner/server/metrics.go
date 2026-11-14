package server

// 记录 LLM / 工具域的最小指标（spec §12.3）。服务自己的指标必须落在骨架的
// SPv2 注册表上，才会出现在 /metrics 里。

import (
	"context"
	"errors"
	"time"

	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) currentLLMModel() string {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.llmModel
}

// metricsRegistry 只在服务骨架存在时返回注册表：包内单测会直接构造 &Server{}（无骨架），
// 指标必须变成 no-op 而不是让单测崩掉。
func (s *Server) metricsRegistry() *spv2.Registry {
	if s == nil || s.Service == nil {
		return nil
	}
	return s.Service.Metrics()
}

func (s *Server) recordLLM(model string, start time.Time, usage llm.Usage, err error) {
	recordLLMTo(s.metricsRegistry(), model, time.Since(start), usage, err)
}

// recordLLMTo 是纯函数形态：包内单测可以直接喂一个 Registry，不依赖服务骨架。
func recordLLMTo(reg *spv2.Registry, model string, elapsed time.Duration, usage llm.Usage, err error) {
	if reg == nil {
		return
	}
	if model == "" {
		model = "unknown"
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	reg.Counter("sump_llm_requests_total", "LLM requests", map[string]string{"model": model, "outcome": outcome}, 1)
	reg.Observe("sump_llm_latency_seconds", "LLM latency", "s", map[string]string{"model": model}, elapsed.Seconds())
	if usage.PromptTokens > 0 {
		reg.Counter("sump_llm_tokens_total", "LLM tokens", map[string]string{"model": model, "kind": "prompt"}, float64(usage.PromptTokens))
	}
	if usage.CompletionTokens > 0 {
		reg.Counter("sump_llm_tokens_total", "LLM tokens", map[string]string{"model": model, "kind": "completion"}, float64(usage.CompletionTokens))
	}
}

// chatLLM 是所有 LLM 调用的唯一入口：统一记请求/延迟/token，避免有的路径漏记。
func (s *Server) chatLLM(ctx context.Context, req llm.ChatRequest) (llm.Message, error) {
	msg, _, err := s.chatLLMWithUsage(ctx, req)
	return msg, err
}

// chatLLMWithUsage 同 chatLLM，但把 token 用量交给调用方（租约扣减要用它，不能另开一条调用路径）。
func (s *Server) chatLLMWithUsage(ctx context.Context, req llm.ChatRequest) (llm.Message, llm.Usage, error) {
	client := s.llmClient()
	if client == nil {
		return llm.Message{}, llm.Usage{}, errors.New("LLM 未配置")
	}
	model := req.Model
	if model == "" {
		model = s.currentLLMModel()
	}
	start := time.Now()
	res, err := client.Chat(ctx, req)
	s.recordLLM(model, start, res.Usage, err)
	return res.Message, res.Usage, err
}

// chatLLMStreamWithUsage 是流式路径的唯一入口：支持 StreamingClient 就用 ChatStream，
// 否则退回 Chat（同一个指标/用量口径，不另开统计）。
func (s *Server) chatLLMStreamWithUsage(ctx context.Context, req llm.ChatRequest, onDelta func(string)) (llm.Message, llm.Usage, error) {
	client := s.llmClient()
	if client == nil {
		return llm.Message{}, llm.Usage{}, errors.New("LLM 未配置")
	}
	model := req.Model
	if model == "" {
		model = s.currentLLMModel()
	}
	start := time.Now()
	if streaming, ok := client.(llm.StreamingClient); ok {
		res, err := streaming.ChatStream(ctx, req, onDelta)
		s.recordLLM(model, start, res.Usage, err)
		return res.Message, res.Usage, err
	}
	res, err := client.Chat(ctx, req)
	s.recordLLM(model, start, res.Usage, err)
	return res.Message, res.Usage, err
}

func (s *Server) recordTool(tool string, start time.Time, err error) {
	recordToolTo(s.metricsRegistry(), tool, time.Since(start), err)
}

func recordToolTo(reg *spv2.Registry, tool string, elapsed time.Duration, err error) {
	if reg == nil {
		return
	}
	if tool == "" {
		tool = "unknown"
	}
	outcome := "ok"
	if err != nil {
		outcome = "error"
	}
	reg.Counter("sump_tool_calls_total", "Tool calls", map[string]string{"tool": tool, "outcome": outcome}, 1)
	reg.Observe("sump_tool_duration_seconds", "Tool call latency", "s", map[string]string{"tool": tool}, elapsed.Seconds())
}
