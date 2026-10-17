package llm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client 是 LLM 客户端抽象（生产走 HTTP，测试用 Fake）。
type Client interface {
	// Chat 发送一次补全请求，返回 assistant 消息。
	Chat(ctx context.Context, req ChatRequest) (Message, error)
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
func (c *DeepSeekClient) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	if c.BaseURL == "" {
		return Message{}, fmt.Errorf("llm: base URL 未配置")
	}
	if req.Model == "" {
		req.Model = c.Model
	}
	body, err := buildRequestBody(req)
	if err != nil {
		return Message{}, fmt.Errorf("llm: 编码请求失败: %w", err)
	}

	url := c.BaseURL + "/chat/completions"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	resp, err := c.HTTP.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("llm: 请求失败: %w", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return Message{}, fmt.Errorf("llm: 读取响应失败: %w", err)
	}

	var parsed ChatResponse
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return Message{}, fmt.Errorf("llm: 响应解析失败 (HTTP %d): %.300s", resp.StatusCode, raw)
	}
	if resp.StatusCode != http.StatusOK {
		if parsed.Error != nil && parsed.Error.Message != "" {
			return Message{}, fmt.Errorf("llm: HTTP %d: %s", resp.StatusCode, parsed.Error.Message)
		}
		return Message{}, fmt.Errorf("llm: HTTP %d: %.300s", resp.StatusCode, raw)
	}
	if len(parsed.Choices) == 0 {
		return Message{}, fmt.Errorf("llm: 响应没有 choices")
	}
	return parsed.Choices[0].Message, nil
}
