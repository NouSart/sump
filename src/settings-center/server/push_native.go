package server

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// pushAttemptSPv2 优先用目标 /spv2 下发 configure；目标没有原生入口时返回 handled=false 回退 v1。
func (s *Server) pushAttemptSPv2(service string, card protocol.ServiceCard, items []protocol.SettingView, idxs []int, results []protocol.SettingResult) ([]int, bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, spv2PushAddr(card.Addr))
	if err != nil {
		return nil, false, nil
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
	return remaining, true, lastErr
}

func spv2PushAddr(addr string) string {
	if strings.HasSuffix(addr, "/ws") {
		return strings.TrimSuffix(addr, "/ws") + "/spv2"
	}
	if strings.HasSuffix(addr, "/spv2") {
		return addr
	}
	return strings.TrimSuffix(addr, "/") + "/spv2"
}
