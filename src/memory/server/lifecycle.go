package server

// 生产 memory 的周期维护（spec §6.9）：memory 是 work 账本的 owner，sweep 由它跑，
// reasoner 只上报进度、不拥有 sweep。

import (
	"context"
	"time"
)

// sweepInterval 是 spec §6.9 的默认 30s；抽成变量便于测试注入。
var sweepInterval = 30 * time.Second

// Start 覆盖骨架启动：先启动服务，再起 work sweep 后台循环。
// 后台循环监听启动 ctx（进程生命周期）；ctx 结束即退出，不泄漏 goroutine。
func (s *Server) Start(ctx context.Context) error {
	if err := s.Service.Start(ctx); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(sweepInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				s.sweepWorks(context.Background())
			}
		}
	}()
	return nil
}
