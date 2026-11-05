// Package adapter 提供 v1（github.com/NouSart/sump/protocol）与
// SPv2.1 帧之间的最小映射，供迁移适配器使用。
//
// 目前只映射 v1 的 jump / response；其余信封类型以及 SPv2.1 的 event / stream
// 暂不迁移，调用方会拿到明确错误而不是静默降级。
package adapter

import (
	"fmt"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// FromV1 把 v1 信封映射为 SPv2.1 帧：
//
//   - jump → call：action 变 method，input 原样作为 body；
//   - response → reply：ok/error 变 status，data 原样作为 body，错误串落为 internal。
//
// 其余 type 返回错误（迁移适配器暂不映射）。
func FromV1(env protocol.Envelope) (spv2.Frame, error) {
	switch env.Type {
	case protocol.TypeJump:
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			return spv2.Frame{}, fmt.Errorf("解析 v1 jump payload 失败: %w", err)
		}
		f := spv2.Frame{
			ID:          env.ID,
			Type:        spv2.TypeCall,
			Work:        workOf(env),
			From:        spv2.ServiceRef(env.From),
			To:          spv2.ServiceRef(env.To),
			Method:      jp.Action,
			Body:        []byte(jp.Input),
			ContentType: spv2.ContentTypeMsgPack,
		}
		f.Normalize()
		if err := f.Validate(); err != nil {
			return spv2.Frame{}, err
		}
		return f, nil

	case protocol.TypeResponse:
		var rp protocol.ResponsePayload
		if err := env.DecodePayload(&rp); err != nil {
			return spv2.Frame{}, fmt.Errorf("解析 v1 response payload 失败: %w", err)
		}
		f := spv2.Frame{
			Type:        spv2.TypeReply,
			Work:        workOf(env),
			From:        spv2.ServiceRef(env.From),
			To:          spv2.ServiceRef(env.To),
			ReplyTo:     env.ID,
			ContentType: spv2.ContentTypeMsgPack,
			Body:        []byte(rp.Data),
		}
		if rp.OK {
			f.Status = spv2.StatusOK
		} else {
			f.Status = spv2.StatusError
			f.Error = spv2.ErrCode(spv2.CodeInternal, rp.Error, false, nil)
		}
		f.Normalize()
		if err := f.Validate(); err != nil {
			return spv2.Frame{}, err
		}
		return f, nil

	default:
		return spv2.Frame{}, fmt.Errorf("v1 信封类型 %q 暂不映射到 SPv2.1", env.Type)
	}
}

// ToV1 把 SPv2.1 帧映射回 v1 信封：
//
//   - call → jump：method 变 action，body 原样作为 input；
//   - reply → response：status 变 ok，error.message 变错误串。
//
// event / stream 返回错误（迁移适配器暂不映射）。
func ToV1(f spv2.Frame) (protocol.Envelope, error) {
	switch f.Type {
	case spv2.TypeCall:
		env, err := protocol.NewEnvelope(protocol.TypeJump, f.From.ID, f.To.ID, f.Work, protocol.JumpPayload{
			Action: f.Method,
			Input:  f.Body,
		})
		if err != nil {
			return protocol.Envelope{}, fmt.Errorf("构造 v1 jump 信封失败: %w", err)
		}
		env.ID = f.ID
		env.V = protocol.ProtocolVersion
		return env, nil

	case spv2.TypeReply:
		payload := protocol.ResponsePayload{
			OK:   f.Status == spv2.StatusOK,
			Data: f.Body,
		}
		if !payload.OK && f.Error != nil {
			payload.Error = f.Error.Message
		}
		env, err := protocol.NewEnvelope(protocol.TypeResponse, f.From.ID, f.To.ID, f.Work, payload)
		if err != nil {
			return protocol.Envelope{}, fmt.Errorf("构造 v1 response 信封失败: %w", err)
		}
		env.ID = f.ReplyTo
		env.V = protocol.ProtocolVersion
		return env, nil

	default:
		return protocol.Envelope{}, fmt.Errorf("SPv2.1 帧类型 %q 暂不映射到 v1", f.Type)
	}
}

// workOf 取映射后的 work：优先沿用 v1 的 trace，缺失时退回消息 id。
func workOf(env protocol.Envelope) string {
	if env.Trace != "" {
		return env.Trace
	}
	return env.ID
}
