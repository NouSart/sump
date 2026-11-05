package spv2

import (
	"encoding/json"

	"github.com/vmihailenco/msgpack/v5"
)

// Marshal 编码帧。
func Marshal(f Frame) ([]byte, error) {
	return msgpack.Marshal(f)
}

// Unmarshal 解码帧。
func Unmarshal(data []byte) (Frame, error) {
	var f Frame
	if err := msgpack.Unmarshal(data, &f); err != nil {
		return Frame{}, err
	}
	return f, nil
}

// EncodeBody 编码 body；[]byte 原样返回。
func EncodeBody(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	if b, ok := v.([]byte); ok {
		return b, nil
	}
	if raw, ok := v.(msgpack.RawMessage); ok {
		return raw, nil
	}
	return msgpack.Marshal(v)
}

// DecodeBody 解码 body 到 dst。
func (f Frame) DecodeBody(dst any) error {
	return DecodeBody(f.Body, dst)
}

// DecodeBody 解码 bytes 到 dst。
func DecodeBody(raw []byte, dst any) error {
	if len(raw) == 0 || dst == nil {
		return nil
	}
	return msgpack.Unmarshal(raw, dst)
}

// DecodeJSONBody 在 content_type=json 时使用。
func (f Frame) DecodeJSONBody(dst any) error {
	if len(f.Body) == 0 || dst == nil {
		return nil
	}
	return json.Unmarshal(f.Body, dst)
}
