package spv2

import (
	"context"
	"fmt"
	"runtime/debug"
	"time"
)

// 健康探针状态。
const (
	HealthDegraded = "degraded"
	HealthFail     = "fail"
)

// HealthChecker 探测一个依赖。返回值必须带 Name 和 Status；LatencyMS 由 Server 实测填充。
type HealthChecker func(context.Context) HealthCheck

// healthChecks 按注册顺序执行探针并汇总总体状态。
func (s *Server) healthChecks(ctx context.Context) ([]HealthCheck, string) {
	if len(s.opts.HealthChecks) == 0 {
		return nil, StatusOK
	}
	if d := s.opts.HealthTimeout; d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	overall := StatusOK
	checks := make([]HealthCheck, 0, len(s.opts.HealthChecks))
	for i, checker := range s.opts.HealthChecks {
		name := fmt.Sprintf("check_%d", i+1)
		started := time.Now()
		check := s.probeHealth(ctx, checker)
		if check.Name != "" {
			name = check.Name
		}
		status := check.Status
		switch status {
		case StatusOK:
		case HealthDegraded:
			if overall == StatusOK {
				overall = HealthDegraded
			}
		default:
			status = HealthFail
			overall = HealthFail
		}
		checks = append(checks, HealthCheck{Name: name, Status: status, LatencyMS: time.Since(started).Milliseconds()})
	}
	return checks, overall
}

// probeHealth 在探针返回与 ctx 到期之间取先到者：探针不尊重 ctx 时，健康检查也必须在
// 上限内给出 fail，不能让 /readyz 或 health 调用挂住（spec §9.3「不得挂住」）。
func (s *Server) probeHealth(ctx context.Context, checker HealthChecker) HealthCheck {
	ch := make(chan HealthCheck, 1)
	go func() { ch <- s.runHealthChecker(ctx, checker) }()
	select {
	case check := <-ch:
		return check
	case <-ctx.Done():
		return HealthCheck{Status: HealthFail}
	}
}

// runHealthChecker 把单个探针的 panic 收成一个 fail，避免健康检查把进程或连接带崩。
func (s *Server) runHealthChecker(ctx context.Context, checker HealthChecker) (check HealthCheck) {
	// 构造器已拒绝 nil 探针（NewServer），这里不重复防御：不可达的分支只会变成
	// 「改坏了也没人发现」的死代码。
	defer func() {
		if r := recover(); r != nil {
			s.logger.Printf("health 探针 panic name=%q: %v\n%s", check.Name, r, debug.Stack())
			check = HealthCheck{Name: check.Name, Status: HealthFail}
		}
	}()
	return checker(ctx)
}
