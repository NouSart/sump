package server

import (
	"context"
	"time"
)

// memory ↔ 执行服务（owner）之间的可注入契约（S-001 §3.0.2 / §3.4 / §3.5 / §5.4）。
//
// 为什么是接口：执行真相在 owner_service，memory 只保存观测与汇总（INV-6）。
// 控制（pause / resume / cancel）与对账（Status）都必须问 owner，不能本地假装成功
// （INV-4 无静默失败）。把这一组调用收敛成一个可注入接口，既让生产实现（经 SPv2
// 调 owner_service）与 memory 的状态机解耦，也让测试能用 fake owner 覆盖故障矩阵：
// 报错、超时、ACK 丢失、离线、recovery_complete=false。
//
// Server 上的客户端是**可选**的（默认 nil）。nil 不是“忘了接”，而是 P1 的显式边界：
//   - pause / resume 回 unavailable，且不改状态；
//   - call_status 的 owner_recovery_complete 恒 false，不把不认识的 call 判 orphaned；
//   - cancel 仍本地执行（不可暂停调用标 remote_may_continue）。
//
// 注入非 nil 客户端后，成功/失败一律以 owner 的回答为准（见 calls_v2.go 的三种控制分支）。
//
// 实现约定（生产实现与 fake 都必须遵守）：
//   - ctx 取消 / 超时后必须尽快返回，不得把 ctx 错误吞成 nil；
//   - Pause 成功时应返回 owner 确认的执行维 "paused"；Resume 成功时应返回 "running"。
//     返回空串表示“已确认但未带状态维”，memory 按约定值处理（paused / running）；
//     返回其它值视为未确认（不能凭一个含糊回答写 paused / running）。
//   - Status 的 states 只包含 owner 认识的 call（call_id → 执行维取值）；
//     recoveryComplete=false 表示 owner 还未恢复完持久受理记录（启动早期），
//     此时“不认识该 call”不能判成 orphaned（S-001 §3.5 规则 1）。
type OwnerClient interface {
	// Status 按 owner_service 批量询问 call 的执行状态（对账，S-001 §3.5）。
	// states 只包含 owner 认识的 id；recoveryComplete 是 owner 的持久受理记录是否已恢复完整。
	Status(ctx context.Context, service string, ids []string) (states map[string]string, recoveryComplete bool, err error)
	// Pause 请求 owner 暂停一次调用；返回 owner 确认后的执行维（成功 = "paused"）。
	Pause(ctx context.Context, service, callID string) (exec string, err error)
	// Resume 请求 owner 恢复一次调用；返回 owner 确认后的执行维（成功 = "running"）。
	Resume(ctx context.Context, service, callID string) (exec string, err error)
	// Cancel 请求 owner 取消一次调用（只对可暂停调用有意义）。
	Cancel(ctx context.Context, service, callID string) (exec string, err error)
}

const (
	// defaultOwnerControlTimeout 是控制握手（pause / resume / cancel）的等待上限，
	// 调用方带 ttl_ms 时以它为准。对齐 S-001 §13 的默认 60000ms。
	defaultOwnerControlTimeout = 60 * time.Second
	// ownerStatusTimeout 是单次对账读取的等待上限（配置默认，不是协议不变量）。
	// 对账是后台周期动作，挂太久会拖住下一轮；这里的上限只约束 memory 自己等多久。
	ownerStatusTimeout = 10 * time.Second
)

// ownerControlContext 给一次控制握手加上等待上限：调用方带 ttl_ms 时按它，
// 缺省用 memory 的默认上限——不能无限挂住 agent 的同步调用。
func ownerControlContext(ctx context.Context, ttlMS int64) (context.Context, context.CancelFunc) {
	wait := defaultOwnerControlTimeout
	if ttlMS > 0 {
		wait = time.Duration(ttlMS) * time.Millisecond
	}
	return context.WithTimeout(ctx, wait)
}

// ownerStatusContext 给单次对账读取加上等待上限（不覆盖调用方更短的 deadline）。
func ownerStatusContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, ownerStatusTimeout)
}

// ownerConfirmed 判断 owner 的回答是否足以确认目标执行维：
// 明确返回目标值，或返回空串（已确认但未带状态维）才算确认。
func ownerConfirmed(exec, want string) bool {
	return exec == "" || exec == want
}
