package spv2

import "fmt"

func validFromKind(k string) bool {
	switch k {
	case "human", "agent", "service", "system":
		return true
	}
	return false
}

func validToKind(k string) bool {
	switch k {
	case "service", "agent", "work", "capability":
		return true
	}
	return false
}

// Validate 校验帧的结构与语义。
func (f Frame) Validate() error {
	if f.V != ProtocolVersion {
		return ErrCode(CodeInvalidArgument, fmt.Sprintf("v 必须是 %s，收到 %q", ProtocolVersion, f.V), false, nil)
	}
	if f.ID == "" {
		return ErrCode(CodeInvalidArgument, "id 必填", false, nil)
	}
	switch f.Type {
	case TypeCall, TypeReply, TypeEvent, TypeStream:
	default:
		return ErrCode(CodeInvalidArgument, "type 非法: "+f.Type, false, nil)
	}
	if f.Work == "" {
		return ErrCode(CodeInvalidArgument, "work 必填", false, nil)
	}
	if !validFromKind(f.From.Kind) || f.From.ID == "" {
		return ErrCode(CodeInvalidArgument, "from 非法", false, nil)
	}
	if !validToKind(f.To.Kind) || f.To.ID == "" {
		return ErrCode(CodeInvalidArgument, "to 非法", false, nil)
	}
	if f.To.Kind == "capability" {
		return ErrCode(CodeNotSupported, "capability 在 v2.1 未设计", false, nil)
	}
	if err := validateAuthority(f.From); err != nil {
		return err
	}
	switch f.Type {
	case TypeCall:
		if f.Method == "" {
			return ErrCode(CodeInvalidArgument, "call.method 必填", false, nil)
		}
	case TypeReply:
		if f.ReplyTo == "" {
			return ErrCode(CodeInvalidArgument, "reply.reply_to 必填", false, nil)
		}
		if f.Status != StatusOK && f.Status != StatusError {
			return ErrCode(CodeInvalidArgument, "reply.status 必须是 ok/error", false, nil)
		}
		if f.Status == StatusError && f.Error == nil {
			return ErrCode(CodeInvalidArgument, "error reply 必须带 error", false, nil)
		}
		if f.Status == StatusOK && f.Error != nil {
			return ErrCode(CodeInvalidArgument, "ok reply 不得带 error", false, nil)
		}
	case TypeEvent:
		if f.Method == "" {
			return ErrCode(CodeInvalidArgument, "event.method 必填", false, nil)
		}
	case TypeStream:
		if f.ReplyTo == "" || f.Stream == nil {
			return ErrCode(CodeInvalidArgument, "stream 必须带 reply_to 与 stream", false, nil)
		}
		if f.Stream.Seq < 1 {
			return ErrCode(CodeInvalidArgument, "stream.seq 必须 >=1", false, nil)
		}
		if f.Stream.End {
			if f.Status != StatusOK && f.Status != StatusError {
				return ErrCode(CodeInvalidArgument, "stream.end 必须带 status", false, nil)
			}
		} else if f.Status != "" || f.Error != nil {
			return ErrCode(CodeInvalidArgument, "非终态 stream 不得带 status/error", false, nil)
		}
	}
	return nil
}

func validateAuthority(from Ref) error {
	switch from.Kind {
	case "human":
		if from.Authority != AuthorityOwner && from.Authority != AuthorityNone {
			return ErrCode(CodeInvalidArgument, "human authority 只能是 owner/none", false, nil)
		}
	case "agent":
		if from.Authority != AuthorityNone && from.Authority != AuthorityDelegated {
			return ErrCode(CodePermissionDenied, "agent authority 只能是 none/delegated", false, nil)
		}
		if from.Authority == AuthorityDelegated {
			return ErrCode(CodeNotSupported, "delegated 在 v2.1 未设计", false, nil)
		}
	case "service", "system":
		if from.Authority != AuthorityNone {
			return ErrCode(CodePermissionDenied, "service/system authority 必须是 none", false, nil)
		}
	}
	return nil
}

// ValidateTo 校验目标是否就是本服务。
func ValidateTo(f Frame, self Ref) error {
	if f.To.Kind == "service" && f.To.ID != self.ID {
		return ErrCode(CodeInvalidArgument, fmt.Sprintf("消息目标 %s 不是本服务 %s", f.To.ID, self.ID), false, nil)
	}
	return nil
}
