// Package spv2 是 SPv2.1（SUMP Protocol v2.1）的参考实现。
//
// 线协议只有一个帧：header + 不透明 body。work 是唯一的工作名词；
// lease 是预算；from.authority 表达权限；memory 是唯一 work 登记处。
package spv2

import (
	"time"
)

// ProtocolVersion 是 SPv2.1 的线协议版本。
const ProtocolVersion = "spv2.1"

// 帧类型。
const (
	TypeCall   = "call"
	TypeReply  = "reply"
	TypeEvent  = "event"
	TypeStream = "stream"
)

// 状态。
const (
	StatusOK    = "ok"
	StatusError = "error"
)

// 默认内容类型。
const (
	ContentTypeMsgPack = "application/msgpack"
	ContentTypeJSON    = "application/json"
	ContentTypeText    = "text/plain"
	ContentTypeBinary  = "application/octet-stream"
)

// Ref 是通用引用：来源或目标。
type Ref struct {
	Kind      string `msgpack:"kind" json:"kind"`
	ID        string `msgpack:"id" json:"id"`
	Authority string `msgpack:"authority,omitempty" json:"authority,omitempty"`
	Tenant    string `msgpack:"tenant,omitempty" json:"tenant,omitempty"`
}

// Authority 取值。
const (
	AuthorityOwner     = "owner"
	AuthorityRuntime   = "runtime"
	AuthorityNone      = "none"
	AuthorityDelegated = "delegated"
)

// Ref 构造器。
func HumanRef(id string) Ref   { return Ref{Kind: "human", ID: id, Authority: AuthorityOwner} }
func AgentRef(id string) Ref   { return Ref{Kind: "agent", ID: id, Authority: AuthorityNone} }
func ServiceRef(id string) Ref { return Ref{Kind: "service", ID: id, Authority: AuthorityNone} }
func SystemRef(id string) Ref  { return Ref{Kind: "system", ID: id, Authority: AuthorityNone} }
func WorkRef(id string) Ref    { return Ref{Kind: "work", ID: id} }
func CapabilityRef(id string) Ref {
	return Ref{Kind: "capability", ID: id}
}

// WithAuthority 返回带 authority 的副本。
func (r Ref) WithAuthority(a string) Ref {
	r.Authority = a
	return r
}

// Stream 是流式响应块的位置。
type Stream struct {
	ID  string `msgpack:"id" json:"id"`
	Seq int64  `msgpack:"seq" json:"seq"`
	End bool   `msgpack:"end" json:"end"`
}

// Error 是结构化错误。
type Error struct {
	Code      string         `msgpack:"code" json:"code"`
	Message   string         `msgpack:"message" json:"message"`
	Retryable bool           `msgpack:"retryable" json:"retryable"`
	Details   map[string]any `msgpack:"details,omitempty" json:"details,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// Frame 是 SPv2.1 的唯一线上单元。
type Frame struct {
	V              string  `msgpack:"v" json:"v"`
	ID             string  `msgpack:"id" json:"id"`
	Type           string  `msgpack:"type" json:"type"`
	Work           string  `msgpack:"work" json:"work"`
	From           Ref     `msgpack:"from" json:"from"`
	To             Ref     `msgpack:"to" json:"to"`
	Method         string  `msgpack:"method,omitempty" json:"method,omitempty"`
	ReplyTo        string  `msgpack:"reply_to,omitempty" json:"reply_to,omitempty"`
	ContentType    string  `msgpack:"content_type,omitempty" json:"content_type,omitempty"`
	Schema         string  `msgpack:"schema,omitempty" json:"schema,omitempty"`
	IdempotencyKey string  `msgpack:"idempotency_key,omitempty" json:"idempotency_key,omitempty"`
	Stream         *Stream `msgpack:"stream,omitempty" json:"stream,omitempty"`
	Status         string  `msgpack:"status,omitempty" json:"status,omitempty"`
	Error          *Error  `msgpack:"error,omitempty" json:"error,omitempty"`
	Body           []byte  `msgpack:"body,omitempty" json:"body,omitempty"`
}

// Normalize 补齐默认值（版本、id、内容类型、authority）。
func (f *Frame) Normalize() {
	if f.V == "" {
		f.V = ProtocolVersion
	}
	if f.ID == "" {
		f.ID = NewID()
	}
	if f.ContentType == "" {
		f.ContentType = ContentTypeMsgPack
	}
	if f.From.Authority == "" {
		switch f.From.Kind {
		case "human":
			f.From.Authority = AuthorityOwner
		case "agent":
			f.From.Authority = AuthorityNone
		default:
			f.From.Authority = AuthorityNone
		}
	}
}

// NewCall 构造一个 call 帧。
func NewCall(work string, from, to Ref, method string, body any) (Frame, error) {
	raw, err := EncodeBody(body)
	if err != nil {
		return Frame{}, err
	}
	f := Frame{
		V: ProtocolVersion, ID: NewID(), Type: TypeCall,
		Work: work, From: from, To: to, Method: method,
		ContentType: ContentTypeMsgPack, Body: raw,
	}
	f.Normalize()
	return f, f.Validate()
}

// NewReply 构造成功 reply。
func NewReply(req Frame, from Ref, body any) (Frame, error) {
	raw, err := EncodeBody(body)
	if err != nil {
		return Frame{}, err
	}
	f := Frame{
		V: ProtocolVersion, ID: NewID(), Type: TypeReply,
		Work: req.Work, From: from, To: req.From, ReplyTo: req.ID,
		Status: StatusOK, ContentType: req.ContentType, Body: raw,
	}
	f.Normalize()
	return f, f.Validate()
}

// NewErrorReply 构造失败 reply。
func NewErrorReply(req Frame, from Ref, code, msg string, retryable bool, details map[string]any) Frame {
	f := Frame{
		V: ProtocolVersion, ID: NewID(), Type: TypeReply,
		Work: req.Work, From: from, To: req.From, ReplyTo: req.ID,
		Status: StatusError, ContentType: req.ContentType,
		Error: &Error{Code: code, Message: msg, Retryable: retryable, Details: details},
	}
	f.Normalize()
	return f
}

// NewEvent 构造一个 event。
func NewEvent(work string, from, to Ref, method string, body any) (Frame, error) {
	raw, err := EncodeBody(body)
	if err != nil {
		return Frame{}, err
	}
	f := Frame{
		V: ProtocolVersion, ID: NewID(), Type: TypeEvent,
		Work: work, From: from, To: to, Method: method,
		ContentType: ContentTypeMsgPack, Body: raw,
	}
	f.Normalize()
	return f, f.Validate()
}

// NewStreamFrame 构造一个流式块。
func NewStreamFrame(call Frame, from Ref, streamID string, seq int64, end bool, body any, status string) (Frame, error) {
	raw, err := EncodeBody(body)
	if err != nil {
		return Frame{}, err
	}
	f := Frame{
		V: ProtocolVersion, ID: NewID(), Type: TypeStream,
		Work: call.Work, From: from, To: call.From, ReplyTo: call.ID,
		ContentType: call.ContentType, Stream: &Stream{ID: streamID, Seq: seq, End: end},
		Body: raw,
	}
	if end {
		f.Status = status
		if f.Status == "" {
			f.Status = StatusOK
		}
	}
	f.Normalize()
	return f, f.Validate()
}

// DeadlineFromLease 解析 lease 的 deadline。
func DeadlineFromLease(l Lease) (time.Time, bool) {
	if l.DeadlineAt == "" {
		return time.Time{}, false
	}
	t, err := time.Parse(time.RFC3339, l.DeadlineAt)
	if err != nil {
		return time.Time{}, false
	}
	return t, true
}
