package ref

import (
	"context"
	"errors"
	"time"

	"github.com/NouSart/sump/spv2"
)

// callPeer 发起一次 call 并解码 reply；timeout<=0 时沿用 ctx 的截止时间。
func callPeer(ctx context.Context, client *spv2.Client, from spv2.Ref, workID, toID, method string, body, dst any, timeout time.Duration) error {
	f, err := spv2.NewCall(workID, from, spv2.ServiceRef(toID), method, body)
	if err != nil {
		return spv2.ErrCode(spv2.CodeInternal, toID+"."+method+" 构造 call 失败: "+err.Error(), false, nil)
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	if err := client.CallDecode(ctx, f, dst); err != nil {
		return peerError(toID+"."+method, err)
	}
	return nil
}

// peerError 把客户端错误统一成 SPv2.1 错误码。
func peerError(step string, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return &spv2.Error{Code: e.Code, Message: step + ": " + e.Message, Retryable: e.Retryable, Details: e.Details}
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return spv2.ErrCode(spv2.CodeDeadlineExceeded, step+" 超时", true, nil)
	}
	if errors.Is(err, context.Canceled) {
		return spv2.ErrCode(spv2.CodeCancelled, step+" 被取消", false, nil)
	}
	return spv2.ErrCode(spv2.CodeUnavailable, step+" 调用失败: "+err.Error(), true, nil)
}
