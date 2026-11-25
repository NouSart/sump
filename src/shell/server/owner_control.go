package server

// owner 侧的对账 / 控制面（R-029）：owner_status / owner_control 的服务实现。
//
// 能力边界（不假装成功）：
//   - status：受理句柄 → 执行维（created / running / done / failed / cancelled / unknown）；
//     终态只保留有限窗口，之后不再认识这个句柄；
//   - cancel：有真实执行面（见 job_control.go）——取消失败 / 超时 / 进程状态不可确认就回
//     unknown + outcome_unknown，不用"假成功"换确认；
//   - pause / resume：shell **没有**暂停/恢复路径，回 not_supported。
//
// 这张表只记账，不调度：shell 仍然无队列、不改变"来几条跑几条"的语义；在跑的命令
// 绝不被清理（只清终态），所以 owner_status 不会把在飞任务说成"不认识"。
// 取消句柄也挂在同一条记录上（终态即摘），句柄条数因此与账本同一口径。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

const (
	// ownerTrackRetention 是终态受理记录的保留窗口：对账是事后动作，忘太快会让"刚跑完"
	// 看起来像"从没存在"（memory 会据此判 orphaned）；永久保留则是无界内存。
	ownerTrackRetention = 10 * time.Minute
	// ownerTrackMax 是账本条数硬上限：超了先从最老的终态挤掉，在跑的不动。
	ownerTrackMax = 1024
)

// trackedJob 是一条受理记录的执行维、最近更新时间和取消句柄。
// ctl 只在未终态时非 nil：终态即摘，句柄不会随账本保留窗口一起滞留。
type trackedJob struct {
	exec    string
	updated time.Time
	ctl     *jobControl
}

// track 记录 / 更新一条受理句柄的执行维，并顺手清理过期终态。只记账，不调度。
func (s *Server) track(item, exec string) { s.trackWith(item, exec, nil) }

// trackWith 更新执行维；ctl != nil 时同时挂上取消句柄（受理时用）。
// 写终态时摘掉句柄：控制面不该拿到一个已经收尾的取消器。
func (s *Server) trackWith(item, exec string, ctl *jobControl) {
	now := time.Now()
	s.trackMu.Lock()
	defer s.trackMu.Unlock()
	rec, exists := s.tracked[item]
	if !exists {
		s.trackOrder = append(s.trackOrder, item)
	}
	rec.exec, rec.updated = exec, now
	if ctl != nil {
		rec.ctl = ctl
	}
	if terminalExec(exec) {
		rec.ctl = nil
	}
	s.tracked[item] = rec
	s.pruneTrackedLocked(now)
}

// pruneTrackedLocked 丢掉过期的终态；条数超上限时从最老的终态开始挤。在跑的一条都不动
// （它们排在最前面也不影响后面终态的清理，所以这里不能"见非终态就收工"）。
func (s *Server) pruneTrackedLocked(now time.Time) {
	keep := s.trackOrder[:0]
	for _, id := range s.trackOrder {
		rec, ok := s.tracked[id]
		if !ok {
			continue // 已被前面的分支删掉
		}
		if !terminalExec(rec.exec) {
			keep = append(keep, id)
			continue
		}
		if now.Sub(rec.updated) <= ownerTrackRetention && len(s.tracked) <= ownerTrackMax {
			keep = append(keep, id)
			continue
		}
		delete(s.tracked, id)
	}
	s.trackOrder = keep
}

// trackedRecord 取一条受理记录（执行维 + 取消句柄）；终态过期后视为不认识。
func (s *Server) trackedRecord(item string, now time.Time) (trackedJob, bool) {
	s.trackMu.Lock()
	defer s.trackMu.Unlock()
	rec, ok := s.tracked[item]
	if !ok {
		return trackedJob{}, false
	}
	if terminalExec(rec.exec) && now.Sub(rec.updated) > ownerTrackRetention {
		return trackedJob{}, false
	}
	return rec, true
}

// trackedExec 取一条受理记录的当前执行维；终态过期后视为不认识。
func (s *Server) trackedExec(item string, now time.Time) (string, bool) {
	rec, ok := s.trackedRecord(item, now)
	if !ok {
		return "", false
	}
	return rec.exec, true
}

// terminalExec 判断执行维是否已终结。unknown 也表示执行路径已收尾（只是没确认结果），
// 因此同样参与过期清理、不再持有取消句柄。
func terminalExec(exec string) bool {
	switch exec {
	case protocol.OwnerExecDone, protocol.OwnerExecFailed, protocol.OwnerExecCancelled, protocol.OwnerExecUnknown:
		return true
	default:
		return false
	}
}

// OwnerStatus 回报一批受理句柄的执行维（S-001 §3.5 的 owner 侧对端）。
//
// recoveryComplete 恒 true：shell 不持久化受理记录，启动后没有待恢复的账本；
// 不认识的 id 就真的没有（可能已超出保留窗口或服务重启前受理）。
func (s *Server) OwnerStatus(_ context.Context, ids []string) (map[string]string, bool, error) {
	now := time.Now()
	states := make(map[string]string, len(ids))
	for _, id := range ids {
		if exec, ok := s.trackedExec(id, now); ok {
			states[id] = exec
		}
	}
	return states, true, nil
}

// OwnerControl 执行 pause / resume / cancel。
//
// 先认对象（id）再说能力：不认识的句柄回 not_found，让调用方能把账本收敛掉；
// 认识的句柄上不支持的 op 回 not_supported 并说明原因——不用"假成功"换确认。
// cancel 有真实执行面：见 cancelTracked。
func (s *Server) OwnerControl(ctx context.Context, id, op string, _ time.Duration) (string, string, error) {
	if id == "" {
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "owner_control: id 必填", false, nil)
	}
	rec, ok := s.trackedRecord(id, time.Now())
	if !ok {
		return "", "", spv2.ErrCode(spv2.CodeNotFound,
			fmt.Sprintf("没有这个受理句柄（可能超出 %s 保留窗口，或服务重启前受理）: %s", ownerTrackRetention, id),
			false, nil)
	}
	switch op {
	case protocol.OwnerOpPause, protocol.OwnerOpResume:
		return "", "", spv2.ErrCode(spv2.CodeNotSupported,
			fmt.Sprintf("不支持 %s：shell 的命令一旦开跑只能等它结束、超时或被 cancel，没有暂停/恢复路径: %s", op, id),
			false, nil)
	case protocol.OwnerOpCancel:
		return s.cancelTracked(ctx, id, rec)
	default:
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "未知 op "+op+": "+id, false, nil)
	}
}

// cancelTracked 取消一条已受理的命令，并把「确认过的」结果回报给控制面。
//
// 语义矩阵：
//   - 已终态：如实回它的终态 + already_terminal（没有可取消的执行）；
//   - 在跑 / 已受理未开跑：cancel 生命周期 ctx（执行路径走 killTree），有界等待收尾，
//     再按账本回答——cancelled（进程已确实停下）/ 真实终态（自然跑完）/ unknown +
//     outcome_unknown（终止失败或超时未见收尾，不谎称已停止）。
//
// 等待不用调用方的 ctx 当唯一闸门：ttl 到点也必须给出「未确认」而不是挂住；
// 反过来说，账本里写下的 cancelled 一定来自执行路径的确认，不是这里猜的。
func (s *Server) cancelTracked(ctx context.Context, id string, rec trackedJob) (string, string, error) {
	if terminalExec(rec.exec) {
		return rec.exec, terminalCancelReason(rec.exec), nil
	}
	if rec.ctl == nil {
		// 活着的记录却没有取消句柄：只有手工构造账本才会这样；不冒充「可取消」。
		return protocol.OwnerExecUnknown, cancelReasonUnknown, nil
	}
	rec.ctl.requestCancel()

	wait := s.cancelWait
	if wait <= 0 {
		wait = cancelSettleTimeout
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	settled := false
	select {
	case <-rec.ctl.done:
		settled = true
	case <-timer.C:
	case <-ctx.Done():
		// 与收尾同时发生：done 先看一眼，能确认就按账本回答。
		select {
		case <-rec.ctl.done:
			settled = true
		default:
		}
	}
	if !settled {
		return protocol.OwnerExecUnknown, cancelReasonUnknown, nil
	}

	done, ok := s.trackedRecord(id, time.Now())
	if !ok || !terminalExec(done.exec) {
		// done 关闭前账本必然已有终态（markTerminal 的顺序）；这里只是防御。
		return protocol.OwnerExecUnknown, cancelReasonUnknown, nil
	}
	switch done.exec {
	case protocol.OwnerExecCancelled:
		return protocol.OwnerExecCancelled, "", nil
	case protocol.OwnerExecUnknown:
		return protocol.OwnerExecUnknown, cancelReasonUnknown, nil
	default:
		return done.exec, terminalCancelReason(done.exec), nil
	}
}

// terminalCancelReason 解释「为什么这条 cancel 没有新的取消效果」。
func terminalCancelReason(exec string) string {
	if exec == protocol.OwnerExecUnknown {
		return cancelReasonUnknown
	}
	return cancelReasonTerminal
}
