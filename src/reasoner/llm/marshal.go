package llm

// OpenAI 兼容请求体的组装：普通消息直接字段映射；
// 带图片引用的消息转为多模态格式（content 数组：text 块 + image_url 块）。

import (
	"encoding/json"
	"strings"
)

// imagePlaceholder 是无内联数据时给图片引用的文本占位（让模型仍知道这条消息带过图）。
const imagePlaceholder = "[图片]"

// buildRequestBody 构造 /chat/completions 请求体。
func buildRequestBody(req ChatRequest) ([]byte, error) {
	type requestBody struct {
		Model     string           `json:"model"`
		Messages  []map[string]any `json:"messages"`
		Tools     []Tool           `json:"tools,omitempty"`
		MaxTokens int              `json:"max_tokens,omitempty"`
	}
	msgs := make([]map[string]any, 0, len(req.Messages))
	for _, m := range req.Messages {
		msgs = append(msgs, marshalMessage(m, req.Images))
	}
	return json.Marshal(requestBody{Model: req.Model, Messages: msgs, Tools: req.Tools, MaxTokens: req.MaxTokens})
}

// marshalMessage 序列化一条消息；Images 是 图片 id → data URL。
func marshalMessage(m Message, images map[string]string) map[string]any {
	out := map[string]any{"role": m.Role}
	if m.ToolCallID != "" {
		out["tool_call_id"] = m.ToolCallID
	}
	if len(m.ToolCalls) > 0 {
		out["tool_calls"] = m.ToolCalls
	}

	var inline []any
	for _, id := range m.ImageIDs {
		if dataURL := images[id]; dataURL != "" {
			inline = append(inline, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": dataURL},
			})
		}
	}

	text := m.Content
	if len(inline) > 0 {
		parts := make([]any, 0, len(inline)+1)
		if strings.TrimSpace(text) != "" {
			parts = append(parts, map[string]any{"type": "text", "text": text})
		}
		parts = append(parts, inline...)
		out["content"] = parts
		return out
	}

	// 无内联数据：有图片引用时补文本占位（更早的消息 / 取图失败），避免图片彻底无痕。
	if len(m.ImageIDs) > 0 && !strings.Contains(text, imagePlaceholder) {
		if strings.TrimSpace(text) == "" {
			text = imagePlaceholder
		} else {
			text = text + "\n" + imagePlaceholder
		}
	}
	if text != "" {
		out["content"] = text
	}
	return out
}
