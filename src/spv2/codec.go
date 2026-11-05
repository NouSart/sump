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

// DecodeBody 按 content_type 解码 body 到 dst。
func (f Frame) DecodeBody(dst any) error {
	switch f.ContentType {
	case "", ContentTypeMsgPack:
		return DecodeBody(f.Body, dst)
	case ContentTypeJSON:
		if len(f.Body) == 0 || dst == nil {
			return nil
		}
		return json.Unmarshal(f.Body, dst)
	case ContentTypeText:
		s, ok := dst.(*string)
		if !ok {
			return ErrCode(CodeInvalidArgument, "text body 只能解码到 *string", false, nil)
		}
		*s = string(f.Body)
		return nil
	case ContentTypeBinary:
		b, ok := dst.(*[]byte)
		if !ok {
			return ErrCode(CodeInvalidArgument, "binary body 只能解码到 *[]byte", false, nil)
		}
		*b = append([]byte(nil), f.Body...)
		return nil
	default:
		return ErrCode(CodeInvalidArgument, "不支持的 content_type: "+f.ContentType, false, nil)
	}
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
