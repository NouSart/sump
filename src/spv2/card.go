package spv2

import "fmt"

// Shape 是 method 的形态。
const (
	ShapeCall  = "call"
	ShapeEvent = "event"
)

// Method 是服务卡里的一个方法。
type Method struct {
	Name             string         `msgpack:"name" json:"name"`
	Shape            string         `msgpack:"shape" json:"shape"`
	Version          int            `msgpack:"version" json:"version"`
	Tool             bool           `msgpack:"tool" json:"tool"`
	Idempotent       bool           `msgpack:"idempotent" json:"idempotent"`
	IdempotencyTTLMS int64          `msgpack:"idempotency_ttl_ms,omitempty" json:"idempotency_ttl_ms,omitempty"`
	Streaming        bool           `msgpack:"streaming" json:"streaming"`
	LongRunning      bool           `msgpack:"long_running" json:"long_running"`
	TimeoutMS        int64          `msgpack:"timeout_ms,omitempty" json:"timeout_ms,omitempty"`
	Description      string         `msgpack:"description,omitempty" json:"description,omitempty"`
	Input            map[string]any `msgpack:"input,omitempty" json:"input,omitempty"`
	Output           map[string]any `msgpack:"output,omitempty" json:"output,omitempty"`
}

// ToolName 返回 LLM 工具名。
func (m Method) ToolName(service string) string { return service + "." + m.Name }

// Limits 是服务限制。
type Limits struct {
	MaxBody   int64 `msgpack:"max_body" json:"max_body"`
	MaxStream int   `msgpack:"max_stream" json:"max_stream"`
	// MaxHeader 是帧头（不含 body）上限，spec §3.5 默认 16 KiB；超限拒绝。
	MaxHeader int64 `msgpack:"max_header,omitempty" json:"max_header,omitempty"`
}

// Observability 是可观测性声明。
type Observability struct {
	Health  bool   `msgpack:"health" json:"health"`
	Metrics bool   `msgpack:"metrics" json:"metrics"`
	Format  string `msgpack:"format,omitempty" json:"format,omitempty"`
}

// Card 是服务卡。
type Card struct {
	Protocol      string        `msgpack:"protocol" json:"protocol"`
	Name          string        `msgpack:"name" json:"name"`
	Actor         Ref           `msgpack:"actor" json:"actor"`
	Version       string        `msgpack:"version" json:"version"`
	Description   string        `msgpack:"description,omitempty" json:"description,omitempty"`
	Capabilities  []string      `msgpack:"capabilities,omitempty" json:"capabilities,omitempty"`
	Methods       []Method      `msgpack:"methods" json:"methods"`
	Limits        Limits        `msgpack:"limits" json:"limits"`
	Observability Observability `msgpack:"observability" json:"observability"`
}

// Method 按名字取 method。
func (c Card) Method(name string) (Method, bool) {
	for _, m := range c.Methods {
		if m.Name == name {
			return m, true
		}
	}
	return Method{}, false
}

// Validate 校验卡片。
func (c Card) Validate() error {
	if c.Protocol != ProtocolVersion {
		return fmt.Errorf("card.protocol 必须是 %s，收到 %q", ProtocolVersion, c.Protocol)
	}
	if c.Name == "" || c.Actor.Kind != "service" || c.Actor.ID == "" {
		return fmt.Errorf("card.name 与 service actor 必填")
	}
	seen := map[string]bool{}
	for _, m := range c.Methods {
		if m.Name == "" {
			return fmt.Errorf("method.name 必填")
		}
		if seen[m.Name] {
			return fmt.Errorf("method 重复: %s", m.Name)
		}
		seen[m.Name] = true
		if m.Shape != ShapeCall && m.Shape != ShapeEvent {
			return fmt.Errorf("method %s shape 必须是 call/event", m.Name)
		}
		if m.Tool && len(m.Input) == 0 {
			return fmt.Errorf("method %s 是 tool 但没有 input schema", m.Name)
		}
	}
	return nil
}
