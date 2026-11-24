package store

// call 账本的两维状态机（S-001 §5.1）与 v2 原子落库用到的共用取值。
//
// P1 只带 RecordV2Atomic 真正调用到的部分：补报按 phase 推进账本时要用的
// 常量、哨兵错误与转移判定。call_open / control / 目录 / 图片索引等 handler 级
// API 留在 P3——没有调用方的入口只会变成没人维护的第二套语义。
//
// 观察维 / 执行维 / phase 取值（S-001 §3.9）：
//
//	observe: waiting / not_waiting / closed
//	exec:    created / running / paused / done / failed / cancelled / unknown

import (
	"errors"
	"time"
)

const (
	CallObserveWaiting    = "waiting"
	CallObserveNotWaiting = "not_waiting"
	CallObserveClosed     = "closed"

	CallExecCreated   = "created"
	CallExecRunning   = "running"
	CallExecPaused    = "paused"
	CallExecDone      = "done"
	CallExecFailed    = "failed"
	CallExecCancelled = "cancelled"
	CallExecUnknown   = "unknown"

	CallPhaseDone      = "done"
	CallPhaseFailed    = "failed"
	CallPhaseTimeout   = "timeout"
	CallPhaseCancelled = "cancelled"
)

// 调用账本的哨兵错误。非法状态转移一律用 ErrCallConflict 包裹（%w）。
var (
	ErrCallConflict = errors.New("call 状态冲突")
	ErrCallNotFound = errors.New("call 不存在")
)

// callTimestamp 固定 9 位小数（纳秒），保证字符串排序 = 时间排序。
func callTimestamp() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func btoi(b bool) int {
	if b {
		return 1
	}
	return 0
}

// canObserve 判断观察维转移是否合法；closed 是终态，不允许再打开。
func canObserve(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case CallObserveWaiting:
		return to == CallObserveNotWaiting || to == CallObserveClosed
	case CallObserveNotWaiting:
		// 对账拿到 owner 的确定答案后可以重新回到等待；closed 之后不行。
		return to == CallObserveWaiting || to == CallObserveClosed
	default:
		return false
	}
}

// canExec 判断执行维转移是否合法（S-001 §5.1）。
// 关键约束：created 不能直接 done（没执行过就不能有结果）；cancelled 不能再 running。
func canExec(from, to string) bool {
	if from == to {
		return true
	}
	switch from {
	case CallExecCreated:
		// 本地意图：只能被派发、本地取消，或对账后变成不可判定。
		return to == CallExecRunning || to == CallExecCancelled || to == CallExecUnknown
	case CallExecRunning:
		return to == CallExecPaused || to == CallExecDone || to == CallExecFailed ||
			to == CallExecCancelled || to == CallExecUnknown
	case CallExecPaused:
		return to == CallExecRunning || to == CallExecCancelled || to == CallExecUnknown
	case CallExecUnknown:
		// 对账可以修正为任意真实状态；但不能退回 created（本地意图只属于派发前）。
		return to == CallExecRunning || to == CallExecPaused || to == CallExecDone ||
			to == CallExecFailed || to == CallExecCancelled
	default:
		// done / failed / cancelled 是终态；对账仍可能把它们修正为 unknown。
		return to == CallExecUnknown
	}
}
