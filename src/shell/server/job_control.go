package server

// 执行中命令的取消句柄（owner_control cancel 的执行面；补齐 R-029 遗留的「只记账、没有终止路径」）。
//
// 登记：run() 受理时把句柄挂进受理账本（tracked[item].ctl），与执行维同一条记录；
// 摘除：终态写进账本时清空（trackWith）。所以句柄数量天然受账本口径约束
// （ownerTrackMax / ownerTrackRetention），不另开一份「在跑列表」，避免两套真相。
//
// 取消不是「发个信号就算成功」：exec.CommandContext 的 Cancel 走 killTree（整棵进程树），
// 只有 cmd.Wait 真的收回了进程（ProcessState != nil）且终止调用没报错，才算确认停止；
// 否则控制面回 unknown + outcome_unknown，不把「没确认」说成「已停止」。

import (
	"context"
	"errors"
	"os"
	"sync"
	"time"
)

const (
	// cancelSettleTimeout 是取消后等待执行路径收尾的上限：超过就如实回「没确认」。
	cancelSettleTimeout = 5 * time.Second
	// cancelReasonUnknown 是「取消未获确认」的原因值（调用方按它做保守处理）。
	cancelReasonUnknown = "outcome_unknown"
	// cancelReasonTerminal 是「这条命令已经结束、没有可取消的执行」的原因值。
	cancelReasonTerminal = "already_terminal"
)

// jobControl 是一条已受理命令的取消句柄。
type jobControl struct {
	ctx    context.Context // 整条命令的生命周期（审批 + 执行）；取消 = cancel 它
	cancel context.CancelFunc
	done   chan struct{} // 终态写入账本后关闭（控制面的确认点）
	once   sync.Once

	mu        sync.Mutex
	requested bool  // owner 请求过取消
	canceled  bool  // 执行路径确实因取消而中止（不是超时）
	exited    bool  // cmd.Wait 收回了进程
	killErr   error // 终止尝试的错误（nil = 成功；os.ErrProcessDone = 本来已结束）
}

func newJobControl() *jobControl {
	ctx, cancel := context.WithCancel(context.Background())
	return &jobControl{ctx: ctx, cancel: cancel, done: make(chan struct{})}
}

// requestCancel 请求终止这条命令；可重复调用（幂等）。不等待收尾——等待由控制面做。
func (c *jobControl) requestCancel() {
	c.mu.Lock()
	c.requested = true
	c.mu.Unlock()
	c.cancel()
}

// recordRun 记录执行路径的收尾事实，由 runCommand 在 Wait 返回后写一次。
func (c *jobControl) recordRun(canceled, exited bool, killErr error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.canceled, c.exited, c.killErr = canceled, exited, killErr
}

// cancelState 把执行路径的事实翻译成两种「取消有后果」的结论：
//   - confirmed：请求过取消，且命令确因取消而终止（杀树没报错、进程已收回）；
//   - unconfirmed：请求过取消，但终止失败或进程状态不可确认（不许假成功）。
//
// 两者都为 false 表示取消没有终止命令：进程本来已结束、自然跑完，或先超时了。
func (c *jobControl) cancelState() (confirmed, unconfirmed bool) {
	if c == nil {
		return false, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.requested || !c.canceled {
		return false, false
	}
	if errors.Is(c.killErr, os.ErrProcessDone) {
		return false, false // 请求取消时进程已经结束：不是我们停的，按真实结果记账
	}
	if c.killErr != nil || !c.exited {
		return false, true
	}
	return true, false
}

// finish 放行等待取消确认的一方。必须在终态写进账本之后调用：
// 控制面在 done 关闭后按账本回答，账本必须先有终态。
func (c *jobControl) finish() {
	if c == nil {
		return
	}
	c.once.Do(func() { close(c.done) })
}

// lifecycleCtx 返回这条命令的生命周期上下文；白盒测试直接构造的 job 没有句柄，用 Background。
func (j *job) lifecycleCtx() context.Context {
	if j.ctl != nil {
		return j.ctl.ctx
	}
	return context.Background()
}

// cancelRequested 判断控制面是否在审批 / 执行前请求了取消。
// 生命周期 ctx 不带超时（超时在执行路径上另加），所以 Err 非 nil 只可能来自取消。
func (j *job) cancelRequested() bool {
	return j.ctl != nil && j.ctl.ctx.Err() != nil
}
