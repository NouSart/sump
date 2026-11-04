package protocol

import "github.com/vmihailenco/msgpack/v5"

// Envelope 是所有控制消息的共同外壳（见 docs/concepts/protocol.md 的「传输、帧与信封」）。
type Envelope struct {
	V       int                `msgpack:"v"`                 // 协议版本
	ID      string             `msgpack:"id"`                // 消息 id（请求 → 响应配对）
	Trace   string             `msgpack:"trace,omitempty"`   // 链路 id（同一用户请求的全链共享）
	Boss    string             `msgpack:"boss,omitempty"`    // 老板：本跳“面向谁工作”（结果归属；不强制沿用上游）
	Type    string             `msgpack:"type"`              // 消息类型 = 动作
	From    string             `msgpack:"from"`              // 来源服务
	To      string             `msgpack:"to"`                // 目标服务
	Payload msgpack.RawMessage `msgpack:"payload,omitempty"` // 该动作的数据
}

// NewEnvelope 构造一条消息：自动填协议版本与消息 id；
// trace 为空时自动生成。payload 可为 nil、结构体或 map。
func NewEnvelope(typ, from, to, trace string, payload any) (Envelope, error) {
	raw, err := EncodePayload(payload)
	if err != nil {
		return Envelope{}, err
	}
	if trace == "" {
		trace = NewID()
	}
	return Envelope{
		V:       ProtocolVersion,
		ID:      NewID(),
		Trace:   trace,
		Type:    typ,
		From:    from,
		To:      to,
		Payload: raw,
	}, nil
}

// NewResponse 构造对 req 的响应：沿用请求的 id（配对）与 trace（同链）。
func NewResponse(req Envelope, from string, payload ResponsePayload) (Envelope, error) {
	env, err := NewEnvelope(TypeResponse, from, req.From, req.Trace, payload)
	if err != nil {
		return Envelope{}, err
	}
	env.ID = req.ID
	return env, nil
}

// Marshal 将信封编码为 MessagePack（WS 二进制帧内容）。
func Marshal(env Envelope) ([]byte, error) {
	return msgpack.Marshal(env)
}

// Unmarshal 将二进制帧解码为信封。
func Unmarshal(data []byte) (Envelope, error) {
	var env Envelope
	if err := msgpack.Unmarshal(data, &env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// EncodePayload 将任意结构编码为 payload 字节。
func EncodePayload(v any) (msgpack.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return msgpack.Marshal(v)
}

// DecodePayload 将信封的 payload 解码到 dst。
func (e Envelope) DecodePayload(dst any) error {
	return DecodeRaw(e.Payload, dst)
}

// DecodeRaw 将 payload / data 等 RawMessage 解码到 dst（空数据跳过）。
func DecodeRaw(raw msgpack.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return msgpack.Unmarshal(raw, dst)
}
