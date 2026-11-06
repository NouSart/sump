package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// pushAttemptSPv2 用目标卡片的 /spv2 地址逐键下发 configure；
// 返回仍未送达的下标（服务明确拒绝记为结果，不重试）。
func (s *Server) pushAttemptSPv2(service string, card protocol.ServiceCard, items []protocol.SettingView, idxs []int, results []protocol.SettingResult) ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, spv2PushAddr(card.Addr))
	if err != nil {
		return idxs, fmt.Errorf("拨号失败: %v", err)
	}
	defer c.Close()

	var remaining []int
	var lastErr error
	for _, i := range idxs {
		it := items[i]
		call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(selfServiceName), spv2.ServiceRef(service), "configure",
			protocol.ConfigurePayload{Key: it.Key, Value: it.Value, Default: it.Default, Overridden: it.Overridden})
		if err != nil {
			results[i].Error = "编码失败: " + err.Error()
			continue
		}
		if _, err := c.Call(ctx, call); err != nil {
			if spv2.IsCode(err, spv2.CodeNotFound) {
				results[i].Error = err.Error()
				continue
			}
			if errors.Is(err, spv2.ErrConnClosed) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				remaining = append(remaining, i)
				lastErr = err
				continue
			}
			results[i].Error = err.Error()
			continue
		}
		results[i].OK = true
		if !it.Secret {
			results[i].Value = it.Value
		}
	}
	return remaining, lastErr
}

func spv2PushAddr(addr string) string {
	if strings.HasSuffix(addr, "/spv2") {
		return addr
	}
	return strings.TrimSuffix(addr, "/") + "/spv2"
}
