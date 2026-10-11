// 错误上报（服务 → 设置中心）：
// Report 通过既有中心连接后台上报一条错误（不拨新连接，发出即完）。
// 防刷：本实例共享一道退避闸——1s 起逐次翻倍、封顶 30s；静默 60s 后重新起算。
// 中心视为永远在线：连接断开期间的上报直接丢弃（只记日志），不做补发队列。
package service

import (
	"context"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

const (
	reportMinGap = time.Second      // 首档重报间隔
	reportMaxGap = 30 * time.Second // 退避封顶
	reportQuiet  = 60 * time.Second // 静默重置阈值
)

// reportThrottle 是错误上报的防刷退避（纯逻辑，便于用假时钟测试）。
// 放行间隔序列：立即 → +1s → +2s → +4s → … → +30s（封顶）；静默 60s 后按 1s 重新起算。
type reportThrottle struct {
	last time.Time     // 上次放行时间
	gap  time.Duration // 当前间隔
}

func (t *reportThrottle) allow(now time.Time) bool {
	if t.last.IsZero() {
		t.last = now
		t.gap = reportMinGap
		return true
	}
	if now.Sub(t.last) >= reportQuiet {
		t.gap = reportMinGap
	}
	if now.Sub(t.last) < t.gap {
		return false
	}
	t.last = now
	t.gap *= 2
	if t.gap > reportMaxGap {
		t.gap = reportMaxGap
	}
	return true
}

// Report 向设置中心后台上报一条错误（发出即完，走既有中心连接）。
// 被退避节流、或尚未连接中心时直接丢弃（记日志）——中心被视为永远在线，不做补发。
func (s *Service) Report(kind, message string) {
	now := time.Now()
	s.repMu.Lock()
	allowed := s.repThrottle.allow(now)
	s.repMu.Unlock()
	if !allowed {
		return
	}

	raw, err := protocol.EncodePayload(protocol.ReportPayload{Kind: kind, Message: message})
	if err != nil {
		return
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, s.cfg.Name, "settings-center", "", protocol.JumpPayload{Action: "report", Input: raw})
	if err != nil {
		return
	}

	s.mu.Lock()
	c := s.center
	s.mu.Unlock()
	if c == nil {
		s.logger.Printf("上报未送达（未连接中心，丢弃）: [%s] %s", kind, message)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.Call(ctx, env); err != nil {
			s.logger.Printf("上报未送达: %v（[%s] %s）", err, kind, message)
		}
	}()
}
