package ref

import "github.com/NouSart/sump/spv2"

// 参考链路里的服务名（同时也是帧的 to.id）。
const (
	ServiceMemory   = "memory"
	ServiceTool     = "tool"
	ServiceEntry    = "frontend"
	ServiceReasoner = "reasoner"
)

// ServiceVersion 是参考服务的版本号。
const ServiceVersion = "0.1.0"

// DefaultOwnerID 是请求体没有给出 owner 时的默认人类 owner。
const DefaultOwnerID = "owner"

func baseCard(name, description string, methods []spv2.Method) spv2.Card {
	return spv2.Card{
		Protocol:      spv2.ProtocolVersion,
		Name:          name,
		Actor:         spv2.ServiceRef(name),
		Version:       ServiceVersion,
		Description:   description,
		Capabilities:  []string{"msgpack", "json"},
		Methods:       methods,
		Limits:        spv2.Limits{MaxBody: 1 << 20, MaxStream: 32},
		Observability: spv2.Observability{Health: true, Metrics: true, Format: "spv2.metrics.v1"},
	}
}

func method(name, description string, tool, idempotent bool, input, output map[string]any) spv2.Method {
	return spv2.Method{
		Name:        name,
		Shape:       spv2.ShapeCall,
		Version:     1,
		Tool:        tool,
		Idempotent:  idempotent,
		Streaming:   false,
		LongRunning: false,
		TimeoutMS:   10000,
		Description: description,
		Input:       input,
		Output:      output,
	}
}

// inputSchema 组装一个最小 JSON Schema 对象。
func inputSchema(props map[string]any, required ...string) map[string]any {
	out := map[string]any{"type": "object"}
	if len(props) > 0 {
		out["properties"] = props
	}
	if len(required) > 0 {
		out["required"] = required
	}
	return out
}

// MemoryCard 返回 memory 的服务卡。
func MemoryCard() spv2.Card {
	return baseCard(ServiceMemory, "SPv2.1 参考 memory：work 登记、lease 账本、A2A 队列", []spv2.Method{
		method("context", "同步组装上下文；首次调用幂等创建 work", false, true,
			inputSchema(map[string]any{
				"messages":     map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
				"owner":        map[string]any{"type": "object"},
				"agent":        map[string]any{"type": "object"},
				"entry":        map[string]any{"type": "object"},
				"conversation": map[string]any{"type": "string"},
			}),
			inputSchema(map[string]any{
				"messages": map[string]any{"type": "array"},
				"work":     map[string]any{"type": "object"},
			}, "work")),
		method("work_get", "按 work id 读取 work 行", false, true,
			inputSchema(map[string]any{"work": map[string]any{"type": "string"}}),
			inputSchema(map[string]any{"work": map[string]any{"type": "object"}}, "work")),
		method("cancel", "取消 work（owner 或 runtime）", false, true,
			inputSchema(map[string]any{
				"work":   map[string]any{"type": "string"},
				"reason": map[string]any{"type": "string"},
				"stream": map[string]any{"type": "string"},
			}),
			inputSchema(map[string]any{
				"work":      map[string]any{"type": "object"},
				"cancelled": map[string]any{"type": "boolean"},
			}, "cancelled")),
		method("renew", "续租 work（owner 或 runtime）", false, false,
			inputSchema(map[string]any{
				"work":  map[string]any{"type": "string"},
				"lease": map[string]any{"type": "object"},
			}, "lease"),
			inputSchema(map[string]any{"work": map[string]any{"type": "object"}}, "work")),
		method("lease_charge", "原子扣减 work 的 lease", false, false,
			inputSchema(map[string]any{
				"work":  map[string]any{"type": "string"},
				"delta": map[string]any{"type": "object"},
			}, "delta"),
			inputSchema(map[string]any{"remaining": map[string]any{"type": "object"}}, "remaining")),
		method("message_post", "A2A 消息入队", false, false,
			inputSchema(map[string]any{
				"work":     map[string]any{"type": "string"},
				"from":     map[string]any{"type": "object"},
				"to":       map[string]any{"type": "object"},
				"method":   map[string]any{"type": "string"},
				"reply_to": map[string]any{"type": "string"},
				"body":     map[string]any{"type": "string"},
			}, "method"),
			inputSchema(map[string]any{"id": map[string]any{"type": "string"}}, "id")),
		method("message_claim", "领取一条 pending 消息并设置租期", false, false,
			inputSchema(map[string]any{
				"work":     map[string]any{"type": "string"},
				"lease_ms": map[string]any{"type": "integer"},
			}),
			inputSchema(map[string]any{"message": map[string]any{"type": "object"}})),
		method("message_ack", "确认消息已投递", false, true,
			inputSchema(map[string]any{"id": map[string]any{"type": "string"}}, "id"),
			inputSchema(map[string]any{"acked": map[string]any{"type": "boolean"}}, "acked")),
	})
}

// ToolCard 返回 tool 的服务卡。
func ToolCard() spv2.Card {
	return baseCard(ServiceTool, "SPv2.1 参考 tool：最小 echo", []spv2.Method{
		method("echo", "原样回显 body", true, true,
			inputSchema(map[string]any{
				"text":         map[string]any{"type": "string"},
				"conversation": map[string]any{"type": "string"},
			}, "text"),
			inputSchema(map[string]any{
				"text":         map[string]any{"type": "string"},
				"conversation": map[string]any{"type": "string"},
			})),
	})
}

// EntryCard 返回入口服务的服务卡。
func EntryCard() spv2.Card {
	return baseCard(ServiceEntry, "SPv2.1 参考入口：接收 deliver 并投递给人类通道", []spv2.Method{
		method("deliver", "把最终答复投递给人类入口", false, false,
			inputSchema(map[string]any{
				"conversation": map[string]any{"type": "string"},
				"text":         map[string]any{"type": "string"},
				"final":        map[string]any{"type": "boolean"},
			}, "text"),
			inputSchema(map[string]any{
				"delivered":    map[string]any{"type": "boolean"},
				"conversation": map[string]any{"type": "string"},
			}, "delivered")),
	})
}

// ReasonerCard 返回 reasoner 的服务卡。
func ReasonerCard() spv2.Card {
	return baseCard(ServiceReasoner, "SPv2.1 参考 reasoner：固定流程 context -> echo -> deliver", []spv2.Method{
		method("message", "统一的人类/agent 消息入口（不接 LLM）", true, false,
			inputSchema(map[string]any{
				"text":         map[string]any{"type": "string"},
				"conversation": map[string]any{"type": "string"},
			}, "text"),
			inputSchema(map[string]any{
				"text": map[string]any{"type": "string"},
				"work": map[string]any{"type": "string"},
			}, "text", "work")),
	})
}
