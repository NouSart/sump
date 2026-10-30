package llm

// OpenAI 兼容的对话与工具调用类型（DeepSeek API 同构）。
// 消息/工具调用的基础结构定义在 protocol 包（记忆服务也要托管同一结构），这里做别名。

import "github.com/NouSart/sump/protocol"

type (
	// Message 是一条对话消息（= protocol.Message）。
	Message = protocol.Message
	// ToolCall 是 assistant 发起的一次工具调用（= protocol.ToolCall）。
	ToolCall = protocol.ToolCall
	// FunctionCall 是工具调用的函数名与参数（= protocol.FunctionCall）。
	FunctionCall = protocol.FunctionCall
)

// Tool 是可提供给模型的工具定义。
type Tool struct {
	Type     string       `json:"type"` // "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction 描述一个工具。
type ToolFunction struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"` // JSON Schema
}

// ChatRequest 是一次补全请求。
type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	// MaxTokens 是输出的硬上限（0 = 不指定，交给服务端默认）。
	// 只给"只要一段话"的场景用：不设上限时模型可能写一大篇，白花钱也白等。
	MaxTokens int `json:"-"`
	// Images 是待内联的图片数据（图片 id → data URL）。
	// 携带数据的引用内联为多模态内容；无数据的引用转文本占位（不直接序列化进请求体）。
	Images map[string]string `json:"-"`
}

// ChatResponse 是补全响应（OpenAI 兼容）。
type ChatResponse struct {
	Choices []Choice  `json:"choices"`
	Error   *APIError `json:"error,omitempty"`
}

// Choice 是响应中的一个候选。
type Choice struct {
	Message Message `json:"message"`
}

// APIError 是 API 返回的错误。
type APIError struct {
	Message string `json:"message"`
}
