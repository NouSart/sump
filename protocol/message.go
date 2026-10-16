package protocol

// 对话消息与工具调用类型（跨服务共享的基础结构）。
// LLM 服务用它承载推理上下文；记忆服务用它托管/组装上下文——所以定义在协议层。

// Message 是一条对话消息（LLM 推理上下文的基本单元）。
type Message struct {
	Role       string     `json:"role" msgpack:"role"`                                     // system / user / assistant / tool
	Content    string     `json:"content,omitempty" msgpack:"content,omitempty"`           // 文本内容
	ImageIDs   []string   `json:"image_ids,omitempty" msgpack:"image_ids,omitempty"`       // 图片引用（由 images 服务按需转为 base64）
	FileIDs    []string   `json:"file_ids,omitempty" msgpack:"file_ids,omitempty"`         // 文件引用（files 服务中的 id；LLM 凭 id 调 files__read 自取）
	ToolCalls  []ToolCall `json:"tool_calls,omitempty" msgpack:"tool_calls,omitempty"`     // assistant 发起的工具调用
	ToolCallID string     `json:"tool_call_id,omitempty" msgpack:"tool_call_id,omitempty"` // tool 结果对应哪次调用
}

// ToolCall 是 assistant 发起的一次工具调用。
type ToolCall struct {
	ID       string       `json:"id" msgpack:"id"`
	Type     string       `json:"type" msgpack:"type"` // "function"
	Function FunctionCall `json:"function" msgpack:"function"`
}

// FunctionCall 是工具调用的函数名与参数（arguments 为 JSON 字符串）。
type FunctionCall struct {
	Name      string `json:"name" msgpack:"name"`
	Arguments string `json:"arguments" msgpack:"arguments"`
}
