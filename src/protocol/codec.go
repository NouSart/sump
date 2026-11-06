package protocol

import "github.com/vmihailenco/msgpack/v5"

// EncodePayload 将任意结构编码为 MessagePack 字节（Frame body、嵌套 input 等）。
func EncodePayload(v any) (msgpack.RawMessage, error) {
	if v == nil {
		return nil, nil
	}
	return msgpack.Marshal(v)
}

// DecodeRaw 将 MessagePack 字节解码到 dst（空数据跳过）。
func DecodeRaw(raw msgpack.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return msgpack.Unmarshal(raw, dst)
}
