package ref

import (
	"context"

	"github.com/NouSart/sump/spv2"
)

// Tool 是 service:tool 的参考实现，只提供 echo。
type Tool struct {
	srv *spv2.Server
}

// NewTool 创建 tool 服务。
func NewTool() (*Tool, error) {
	srv, err := spv2.NewServer(ToolCard())
	if err != nil {
		return nil, err
	}
	t := &Tool{srv: srv}
	srv.Handle("echo", t.handleEcho)
	return t, nil
}

// Server 返回底层 SPv2.1 服务端。
func (t *Tool) Server() *spv2.Server { return t.srv }

func (t *Tool) handleEcho(_ context.Context, call *spv2.Call) error {
	if len(call.Frame.Body) == 0 {
		return call.Reply(nil)
	}
	var body any
	if err := call.Frame.DecodeBody(&body); err != nil {
		return spv2.ErrCode(spv2.CodeInvalidArgument, "tool.echo body 无效: "+err.Error(), false, nil)
	}
	return call.Reply(body)
}
