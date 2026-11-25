package server

// owner 侧的对账 / 控制面（R-029）：owner_status / owner_control 的服务实现。
//
// 能力边界（不假装成功）：
//   - status：受理句柄 → 执行维（created / running / done / failed）；终态只保留
//     有限窗口，之后不再认识这个句柄；
//   - cancel / pause / resume：shell **没有**终止或暂停执行中命令的路径（唯一终止手段是
//     exec.timeout_ms 超时），因此一律回 not_supported 并说明原因，不用"假成功"换确认。
//
// 这张表只记账，不调度：shell 仍然无队列、不改变"来几条跑几条"的语义；在跑的命令
// 绝不被清理（只清终态），所以 owner_status 不会把在飞任务说成"不认识"。

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

// trackedJob 是一条受理记录的执行维与最近更新时间。
type trackedJob struct {
	exec    string
	updated time.Time
}

// track 记录 / 更新一条受理句柄的执行维，并顺手清理过期终态。只记账，不调度。
func (s *Server) track(item, exec string) {
	now := time.Now()
	s.trackMu.Lock()
	defer s.trackMu.Unlock()
	if _, exists := s.tracked[item]; !exists {
		s.trackOrder = append(s.trackOrder, item)
	}
	s.tracked[item] = trackedJob{exec: exec, updated: now}
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

// trackedExec 取一条受理记录的当前执行维；终态过期后视为不认识。
func (s *Server) trackedExec(item string, now time.Time) (string, bool) {
	s.trackMu.Lock()
	defer s.trackMu.Unlock()
	rec, ok := s.tracked[item]
	if !ok {
		return "", false
	}
	if terminalExec(rec.exec) && now.Sub(rec.updated) > ownerTrackRetention {
		return "", false
	}
	return rec.exec, true
}

// terminalExec 判断执行维是否已终结（shell 只会写 done / failed；cancelled 留给共用语义）。
func terminalExec(exec string) bool {
	switch exec {
	case protocol.OwnerExecDone, protocol.OwnerExecFailed, protocol.OwnerExecCancelled:
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

// OwnerControl 拒绝 pause / resume / cancel：shell 没有这些路径。
//
// 先认对象（id）再说能力：不认识的句柄回 not_found，让调用方能把账本收敛掉；
// 认识的句柄回 not_supported 并说明"为什么做不到、替代手段是什么"。
func (s *Server) OwnerControl(_ context.Context, id, op string, _ time.Duration) (string, string, error) {
	if _, ok := s.trackedExec(id, time.Now()); !ok {
		return "", "", spv2.ErrCode(spv2.CodeNotFound,
			fmt.Sprintf("没有这个受理句柄（可能超出 %s 保留窗口，或服务重启前受理）: %s", ownerTrackRetention, id),
			false, nil)
	}
	switch op {
	case protocol.OwnerOpPause, protocol.OwnerOpResume:
		return "", "", spv2.ErrCode(spv2.CodeNotSupported,
			fmt.Sprintf("不支持 %s：shell 的命令一旦开跑只能等它结束或超时（exec.timeout_ms），没有暂停/恢复路径: %s", op, id),
			false, nil)
	case protocol.OwnerOpCancel:
		return "", "", spv2.ErrCode(spv2.CodeNotSupported,
			"不支持 cancel：shell 没有终止执行中命令的路径（唯一终止手段是 exec.timeout_ms 超时）；不假装已停止: "+id,
			false, nil)
	default:
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "未知 op "+op+": "+id, false, nil)
	}
}

// terminalExecOfPhase 把补报阶段翻译成执行维：done 才是 done，其余终态（failed / rejected）
// 都是 failed——调用没成功时不能写成 done。
func terminalExecOfPhase(phase string) string {
	if phase == "done" {
		return protocol.OwnerExecDone
	}
	return protocol.OwnerExecFailed
}
