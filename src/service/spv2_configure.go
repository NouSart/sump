package service

import (
	"context"

	"github.com/NouSart/sump/spv2"
)

func (s *Service) nativeConfigure(_ context.Context, call *spv2.Call) error {
	out, err := s.configure(call.Frame.Body)
	if err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	return call.Reply(out)
}
