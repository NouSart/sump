package session

// 本服务的周期维护（spec §6.9）：会话库是 work 账本的 owner，sweep 由它跑，
// 其他服务只上报进度、不拥有 sweep。

import (
	"context"
	"time"
)

// sweepInterval 是 spec §6.9 的默认 30s；抽成变量便于测试注入。
var sweepInterval = 30 * time.Second

// Start 启动 SPv2 服务端（Standalone=true 时不连设置中心），再起 work sweep 与 call 对账
// 两个后台循环。后台循环监听启动 ctx（进程生命周期）；ctx 结束即退出，不泄漏 goroutine。
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
	go func() {
		t := time.NewTicker(callReconcileInterval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				// 每轮独立 ctx：一轮里的 owner 批次各有 ownerStatusTimeout，不会互相拖挂。
				s.reconcileCalls(context.Background())
			}
		}
	}()
	return nil
}
