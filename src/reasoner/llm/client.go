package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Usage 是 provider 回报的 token 用量（缺省时为零值，指标按 0 处理）。
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// ChatResult 是一次补全的结果：assistant 消息 + token 用量。
type ChatResult struct {
	Message Message
	Usage   Usage
}

// Client 是 LLM 客户端抽象（生产走 HTTP，测试用 Fake）。
type Client interface {
	// Chat 发送一次补全请求，返回 assistant 消息与用量。
	Chat(ctx context.Context, req ChatRequest) (ChatResult, error)
}

// StreamingClient 是可选的流式补全能力：ChatStream 在生成过程中回调文本增量，
// 返回完整 ChatResult（含 usage）。调用方用类型断言探测，不支持就退回 Chat。
type StreamingClient interface {
	ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error)
}

// DeepSeekClient 是 OpenAI 兼容的 HTTP 客户端（DeepSeek 及同类供应商）。
type DeepSeekClient struct {
	BaseURL string // 如 https://api.deepseek.com（可带 /v1）
	APIKey  string
	Model   string // 如 deepseek-flash
	HTTP    *http.Client
}

// NewDeepSeekClient 构造客户端（Model 为空时报错由调用方负责检查）。
func NewDeepSeekClient(baseURL, apiKey, model string) *DeepSeekClient {
	return &DeepSeekClient{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Model:   model,
		HTTP:    &http.Client{Timeout: 120 * time.Second},
	}
}

// Chat 实现了 Client。
func (c *DeepSeekClient) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	if c.BaseURL == "" {
		return ChatResult{}, fmt.Errorf("llm: base URL 未配置")
	}
	if req.Model == "" {
		req.Model = c.Model
	}
	body, err := buildRequestBody(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: 编码请求失败: %w", err)
	}

	url := c.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: 读取响应失败: %w", err)
	}

	var parsed ChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return ChatResult{}, fmt.Errorf("llm: 响应解析失败 (HTTP %d): %.300s", resp.StatusCode, raw)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return ChatResult{}, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return ChatResult{}, fmt.Errorf("llm: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	if len(parsed.Choices) == 0 {
		return ChatResult{}, fmt.Errorf("llm: 响应没有 choices")
	}
	return ChatResult{Message: parsed.Choices[0].Message, Usage: parsed.Usage}, nil
}

// ChatStream 实现 StreamingClient：SSE 逐块读取 content 增量并回调；工具调用按 index 拼装。
func (c *DeepSeekClient) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error) {
	if c.BaseURL == "" {
		return ChatResult{}, fmt.Errorf("llm: base URL 未配置")
	}
	if onDelta == nil {
		onDelta = func(string) {}
	}
	if req.Model == "" {
		req.Model = c.Model
	}
	req.Stream = true
	body, err := buildRequestBody(req)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: 编码请求失败: %w", err)
	}
	url := c.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return ChatResult{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return ChatResult{}, fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		return ChatResult{}, fmt.Errorf("llm: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	var content strings.Builder
	var usage Usage
	type callFrag struct {
		id, typ, name string
		args          strings.Builder
	}
	frags := map[int]*callFrag{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   string `json:"content"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Type     string `json:"type"`
						Function struct {
							Name      string `json:"name"`
							Arguments string `json:"arguments"`
						} `json:"function"`
					} `json:"tool_calls"`
				} `json:"delta"`
			} `json:"choices"`
			Usage *Usage `json:"usage"`
		}
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if chunk.Usage != nil {
			usage = *chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				content.WriteString(choice.Delta.Content)
				onDelta(choice.Delta.Content)
			}
			for _, tc := range choice.Delta.ToolCalls {
				frag := frags[tc.Index]
				if frag == nil {
					frag = &callFrag{}
					frags[tc.Index] = frag
				}
				if tc.ID != "" {
					frag.id = tc.ID
				}
				if tc.Type != "" {
					frag.typ = tc.Type
				}
				if tc.Function.Name != "" {
					frag.name += tc.Function.Name
				}
				frag.args.WriteString(tc.Function.Arguments)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return ChatResult{}, fmt.Errorf("llm: 流式读取失败: %w", err)
	}
	msg := Message{Role: "assistant", Content: content.String()}
	for i := 0; i < len(frags); i += 1 {
		frag := frags[i]
		if frag == nil || frag.name == "" {
			continue
		}
		typ := frag.typ
		if typ == "" {
			typ = "function"
		}
		msg.ToolCalls = append(msg.ToolCalls, ToolCall{
			ID: frag.id, Type: typ,
			Function: FunctionCall{Name: frag.name, Arguments: frag.args.String()},
		})
	}
	return ChatResult{Message: msg, Usage: usage}, nil
}
