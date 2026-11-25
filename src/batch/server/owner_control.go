package server

// owner 侧的对账 / 控制面（R-029）：owner_status / owner_control 的服务实现。
//
// batch 的能力与它自己的 control 动作一致（见 batch/README.md「暂停 ≠ 停止」）：
//   - status：账本里的任务（phase → exec）；
//   - pause：可恢复——未开始的项保持 pending，在飞的项跑完；
//   - resume：从断点继续，已完成的项不重跑；
//   - cancel：终态——未开始的项作废，在飞的项跑完。
//
// 「不假装」的落点：cancel 被受理时在飞项还在跑，账本仍是 running，此时回 exec=running
// 并给出 reason（memory 会据此标 remote_may_continue）；等到账本真的转 cancelled，
// 下一次对账才会看到 cancelled。
//
// recoveryComplete 在 recoverAfterRestart 完成前为 false（启动早期）：那时「不认识」不能
// 判 orphaned（S-001 §3.5 规则 1）。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// batchExecOfPhase 把 batch 的阶段翻译成 S-001 的 exec 词汇。
// 未知阶段回 unknown（显式说出"不知道"），不猜 done / failed。
func batchExecOfPhase(phase string) string {
	switch phase {
	case protocol.BatchPhaseAccepted:
		return protocol.OwnerExecCreated
	case protocol.BatchPhaseRunning:
		return protocol.OwnerExecRunning
	case protocol.BatchPhasePaused:
		return protocol.OwnerExecPaused
	case protocol.BatchPhaseDone:
		return protocol.OwnerExecDone
	case protocol.BatchPhaseFailed:
		return protocol.OwnerExecFailed
	case protocol.BatchPhaseCancelled:
		return protocol.OwnerExecCancelled
	default:
		return protocol.OwnerExecUnknown
	}
}

// batchExecTerminal 判断执行维是否已终结。
func batchExecTerminal(exec string) bool {
	return exec == protocol.OwnerExecDone || exec == protocol.OwnerExecFailed || exec == protocol.OwnerExecCancelled
}

// ownerPhase 取任务的当前阶段：内存里的活动任务优先（账本的 phase 在"accepted"之后
// 不会随执行推进而改写，直接读账本会把在跑的任务说成 created），终态任务读账本。
func (s *Server) ownerPhase(id string) (phase string, known bool, err error) {
	j, ok, err := s.store.Job(id)
	if err != nil || !ok {
		return "", ok, err
	}
	phase = j.Phase
	s.mu.Lock()
	live := s.jobs[id]
	s.mu.Unlock()
	if live != nil {
		live.mu.Lock()
		if !live.finished { // 已落终态的内存任务以账本为准（落终态与摘除之间有窗口）
			phase = live.phaseLocked()
		}
		live.mu.Unlock()
	}
	return phase, true, nil
}

// OwnerStatus 回报一批任务句柄的执行维（只回账本里还认识的任务）。
func (s *Server) OwnerStatus(_ context.Context, ids []string) (map[string]string, bool, error) {
	s.ownerMu.Lock()
	complete := s.recovered
	s.ownerMu.Unlock()
	if s.store == nil {
		return nil, complete, spv2.ErrCode(spv2.CodeUnavailable,
			"batch: 任务账本尚未打开（服务未启动）", true, nil)
	}
	states := make(map[string]string, len(ids))
	for _, id := range ids {
		phase, ok, err := s.ownerPhase(id)
		if err != nil {
			return nil, complete, fmt.Errorf("batch: 读任务 %s 失败: %w", id, err)
		}
		if !ok {
			continue
		}
		states[id] = batchExecOfPhase(phase)
	}
	return states, complete, nil
}

// OwnerControl 对单个任务执行 pause / resume / cancel，回账本里的真实阶段。
//
// 不用 control 动作的 affected 计数来说"成功"：owner_control 的确认标准是**账本里的
// 阶段**——pause 成功才回 paused、resume 成功才回 running、cancel 真的转终态才回 cancelled。
func (s *Server) OwnerControl(_ context.Context, id, op string, _ time.Duration) (string, string, error) {
	if s.store == nil {
		return "", "", spv2.ErrCode(spv2.CodeUnavailable,
			"batch: 任务账本尚未打开（服务未启动）", true, nil)
	}
	if _, ok, err := s.store.Job(id); err != nil {
		return "", "", fmt.Errorf("batch: 读任务 %s 失败: %w", id, err)
	} else if !ok {
		return "", "", spv2.ErrCode(spv2.CodeNotFound, "没有这个批量任务（或已过期）: "+id, false, nil)
	}
	var bop, want string
	switch op {
	case protocol.OwnerOpPause:
		bop, want = protocol.BatchOpPause, protocol.OwnerExecPaused
	case protocol.OwnerOpResume:
		bop, want = protocol.BatchOpResume, protocol.OwnerExecRunning
	case protocol.OwnerOpCancel:
		bop, want = protocol.BatchOpCancel, protocol.OwnerExecCancelled
	default:
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "未知 op "+op+": "+id, false, nil)
	}
	if _, err := s.controlOne(bop, id); err != nil {
		return "", "", fmt.Errorf("batch: 任务 %s 的 %s 失败: %w", id, op, err)
	}
	phase, ok, err := s.ownerPhase(id)
	if err != nil {
		return "", "", fmt.Errorf("batch: 读任务 %s 失败: %w", id, err)
	}
	if !ok {
		return "", "", spv2.ErrCode(spv2.CodeNotFound, "没有这个批量任务（或已过期）: "+id, false, nil)
	}
	exec := batchExecOfPhase(phase)
	reason := ""
	switch {
	case exec == want:
		// 刚刚生效，或本来就在目标状态（幂等命中）。
	case op == protocol.OwnerOpResume && exec == protocol.OwnerExecPaused:
		reason = "恢复未生效：全局在飞槽位不足（任务仍是 paused），稍后再 resume"
	case batchExecTerminal(exec):
		reason = fmt.Sprintf("任务已结束（%s），%s 无从生效", exec, op)
	default:
		reason = fmt.Sprintf("已受理 %s：任务当前仍是 %s（在飞项跑完后转 %s）", op, exec, want)
	}
	return exec, reason, nil
}
