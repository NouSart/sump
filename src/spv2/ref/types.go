package ref

import "github.com/NouSart/sump/spv2"

// Message 是 memory.context 里一条上下文消息的最小形状。
// 真实 memory 可以返回更丰富的消息对象；参考链路只保留 role/text。
type Message struct {
	Role string `msgpack:"role,omitempty" json:"role,omitempty"`
	Text string `msgpack:"text,omitempty" json:"text,omitempty"`
}

// ContextRequest 是 memory.context 的请求体（SPv2.1 §7.2）：
// messages/owner/conversation 来自调用方，agent/entry 是参考链路的可选补充。
type ContextRequest struct {
	Messages     []Message `msgpack:"messages,omitempty" json:"messages,omitempty"`
	Owner        spv2.Ref  `msgpack:"owner,omitempty" json:"owner,omitempty"`
	Agent        spv2.Ref  `msgpack:"agent,omitempty" json:"agent,omitempty"`
	Entry        spv2.Ref  `msgpack:"entry,omitempty" json:"entry,omitempty"`
	Conversation string    `msgpack:"conversation,omitempty" json:"conversation,omitempty"`
}

// ContextResponse 是 memory.context 的响应体：上下文消息 + work 行。
type ContextResponse struct {
	Messages []Message `msgpack:"messages" json:"messages"`
	Work     spv2.Work `msgpack:"work" json:"work"`
}

// WorkGetRequest 是 memory.work_get 的请求体；work 为空时取 frame.work。
type WorkGetRequest struct {
	Work string `msgpack:"work,omitempty" json:"work,omitempty"`
}

// WorkGetResponse 是 memory.work_get 的响应体。
type WorkGetResponse struct {
	Work spv2.Work `msgpack:"work" json:"work"`
}

// LeaseChargeRequest 是 memory.lease_charge 的请求体。
type LeaseChargeRequest struct {
	Work  string      `msgpack:"work,omitempty" json:"work,omitempty"`
	Delta spv2.Charge `msgpack:"delta" json:"delta"`
}

// LeaseChargeResponse 是 memory.lease_charge 的响应体。
type LeaseChargeResponse struct {
	Remaining spv2.Remaining `msgpack:"remaining" json:"remaining"`
}

// MessagePostRequest 是 memory.message_post 的请求体。
type MessagePostRequest struct {
	Work    string   `msgpack:"work,omitempty" json:"work,omitempty"`
	From    spv2.Ref `msgpack:"from,omitempty" json:"from,omitempty"`
	To      spv2.Ref `msgpack:"to,omitempty" json:"to,omitempty"`
	Method  string   `msgpack:"method" json:"method"`
	ReplyTo string   `msgpack:"reply_to,omitempty" json:"reply_to,omitempty"`
	Body    []byte   `msgpack:"body,omitempty" json:"body,omitempty"`
}

// MessagePostResponse 是 memory.message_post 的响应体。
type MessagePostResponse struct {
	ID string `msgpack:"id" json:"id"`
}

// MessageClaimRequest 是 memory.message_claim 的请求体。
type MessageClaimRequest struct {
	Work    string `msgpack:"work,omitempty" json:"work,omitempty"`
	LeaseMS int64  `msgpack:"lease_ms,omitempty" json:"lease_ms,omitempty"`
}

// MessageClaimResponse 是 memory.message_claim 的响应体；没有 pending 消息时 message 为 nil。
type MessageClaimResponse struct {
	Message *spv2.QueuedMessage `msgpack:"message,omitempty" json:"message,omitempty"`
}

// MessageAckRequest 是 memory.message_ack 的请求体。
type MessageAckRequest struct {
	ID string `msgpack:"id" json:"id"`
}

// MessageAckResponse 是 memory.message_ack 的响应体。
type MessageAckResponse struct {
	Acked bool `msgpack:"acked" json:"acked"`
}

// CancelRequest 是 memory.cancel 的请求体（SPv2.1 §7.4）。
type CancelRequest struct {
	Work   string `msgpack:"work,omitempty" json:"work,omitempty"`
	Reason string `msgpack:"reason,omitempty" json:"reason,omitempty"`
	Stream string `msgpack:"stream,omitempty" json:"stream,omitempty"`
}

// CancelResponse 是 memory.cancel 的响应体。
type CancelResponse struct {
	Work      spv2.Work `msgpack:"work" json:"work"`
	Cancelled bool      `msgpack:"cancelled" json:"cancelled"`
}

// RenewRequest 是 memory.renew 的请求体（SPv2.1 §7.5）。
type RenewRequest struct {
	Work  string     `msgpack:"work,omitempty" json:"work,omitempty"`
	Lease spv2.Lease `msgpack:"lease" json:"lease"`
}

// RenewResponse 是 memory.renew 的响应体。
type RenewResponse struct {
	Work spv2.Work `msgpack:"work" json:"work"`
}

// MessageRequest 是 reasoner.message 的请求体。
type MessageRequest struct {
	Text         string `msgpack:"text" json:"text"`
	Conversation string `msgpack:"conversation,omitempty" json:"conversation,omitempty"`
}

// MessageResponse 是 reasoner.message 的成功响应体。
type MessageResponse struct {
	Text string `msgpack:"text" json:"text"`
	Work string `msgpack:"work" json:"work"`
}

// EchoRequest 是 tool.echo 的参考载荷。
type EchoRequest struct {
	Text         string `msgpack:"text" json:"text"`
	Conversation string `msgpack:"conversation,omitempty" json:"conversation,omitempty"`
}

// EchoResponse 与 EchoRequest 同形，便于链路上直接回显。
type EchoResponse struct {
	Text         string `msgpack:"text" json:"text"`
	Conversation string `msgpack:"conversation,omitempty" json:"conversation,omitempty"`
}

// DeliverRequest 是 entry.deliver 的请求体（SPv2.1 §7.3）。
type DeliverRequest struct {
	Conversation string `msgpack:"conversation,omitempty" json:"conversation,omitempty"`
	Text         string `msgpack:"text" json:"text"`
	Final        bool   `msgpack:"final,omitempty" json:"final,omitempty"`
}

// DeliverResponse 是 entry.deliver 的响应体。
type DeliverResponse struct {
	Delivered    bool   `msgpack:"delivered" json:"delivered"`
	Conversation string `msgpack:"conversation,omitempty" json:"conversation,omitempty"`
}

// Delivery 是 entry 收到的、等待人类通道消费的一条投递。
type Delivery struct {
	Work         string
	From         spv2.Ref
	Conversation string
	Text         string
	Final        bool
}
