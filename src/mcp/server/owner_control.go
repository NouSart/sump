package server

// owner 侧的对账 / 控制面（R-029）：owner_status / owner_control 的 mcp 实现。
//
// mcp 的「执行」是导入工具调用（SUMP → 外部 MCP 的异步 import），不是导出侧的
// jobs 观察表：导出是外部客户端调 SUMP 工具，既不属于 memory 的调用账本，也没有
// 这里要回报的受理句柄。
//
// 能力边界（不假装成功）：
//   - status：受理句柄 → 执行维。仍在本地观察 = running；本地观察结束 = done /
//     failed / unknown（远端结果没有确认）/ cancelled（本地等待已被撤销）。终态只在
//     保留窗口内可见，之后不再认识这个句柄；
//   - cancel：复用 cancel_import 的本地取消路径，回 exec=unknown +
//     reason=outcome_unknown——远端工具可能仍在执行，mcp 不声称能停掉它；
//     返回 cancelled 等于声称远端已停，会让 memory 的取消确认误判，故用 unknown。
//   - pause / resume：导入没有暂停 / 恢复路径，回 not_supported。
//
// recoveryComplete 恒 true：异步导入只有内存里的观察，进程重启即丢，启动后没有待恢复的
// 账本；「不认识」就是真的不认识（可能超出保留窗口，或重启前受理）。
//
// 调用者身份：OwnerControl 的 hook 接口只拿得到受理句柄，拿不到发起者，所以控制面按
// **句柄**认对象（句柄是随受理回执只发给原调用方的随机 ULID），不再校验原始 from。
// 这与原生 cancel_import 的「只有原 from 能取消」不同，是这里已知的语义差异；内部
// SPv2 网络是可信边界（见 docs/concepts/protocol.md 的「信任边界」）。

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

const (
	// importExecRetention 是终态受理记录的保留窗口：对账是事后动作，忘太快会让「刚跑完」
	// 看起来像「从没存在」（memory 会据此判 orphaned）；永久保留则是无界内存。
	importExecRetention = 10 * time.Minute
	// importExecMax 是账本条数硬上限：超了先从最老的终态挤掉，在跑的不动。
	importExecMax = 1024
	// importCancelReason 是 cancel 成功的固定原因：本地等待已撤销，远端是否停止未知。
	importCancelReason = "outcome_unknown"
)

// importExecRecord 是一条导入受理句柄的执行维与最近更新时间。
type importExecRecord struct {
	exec      string
	updated   time.Time
	cancelled bool // 由 cancel 路径写入：worker 之后的终态不得覆盖
}

// importExecLedger 记账「受理句柄 → 执行维」，只做对账，不调度导入。
// 它是派生数据：丢了只影响事后对账窗口，不影响补报与远端执行。
type importExecLedger struct {
	mu    sync.Mutex
	byID  map[string]importExecRecord
	order []string
}

func newImportExecLedger() *importExecLedger {
	return &importExecLedger{byID: make(map[string]importExecRecord)}
}

// accepted 记录一条刚受理的导入（回执发出前调用）。
func (l *importExecLedger) accepted(id string) {
	l.set(id, protocol.OwnerExecRunning, false)
}

// finished 记录本地观察结束时的执行维；已确认取消的句柄不被 worker 的终态覆盖。
func (l *importExecLedger) finished(id, exec string) {
	l.set(id, exec, false)
}

// cancelUnknown 记录本地等待已被撤销，但远端是否停止未知（不能记成 cancelled）。
func (l *importExecLedger) cancelUnknown(id string) {
	if id == "" {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if _, ok := l.byID[id]; !ok {
		l.order = append(l.order, id)
	}
	l.byID[id] = importExecRecord{exec: protocol.OwnerExecUnknown, updated: now, cancelled: true}
	l.pruneLocked(now)
}

// exec 取一条受理记录的当前执行维；终态过期后视为不认识。
func (l *importExecLedger) exec(id string) (string, bool) {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	rec, ok := l.byID[id]
	return rec.exec, ok
}

func (l *importExecLedger) set(id, exec string, forced bool) {
	if id == "" || exec == "" {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if old, ok := l.byID[id]; ok && !forced && old.cancelled {
		return // 取消已生效：worker 之后的终态不能把「本地等待已撤销」改回去。
	}
	if _, ok := l.byID[id]; !ok {
		l.order = append(l.order, id)
	}
	l.byID[id] = importExecRecord{exec: exec, updated: now}
	l.pruneLocked(now)
}

// pruneLocked 丢掉过期的终态；条数超上限时从最老的终态开始挤。running 的一条都不动
// （它们排在最前面也不影响后面终态的清理，所以这里不能「见非终态就收工」）。
func (l *importExecLedger) pruneLocked(now time.Time) {
	keep := l.order[:0]
	for _, id := range l.order {
		rec, ok := l.byID[id]
		if !ok {
			continue // 已被前面的分支删掉
		}
		if !importExecTerminal(rec.exec) {
			keep = append(keep, id)
			continue
		}
		if now.Sub(rec.updated) <= importExecRetention && len(l.byID) <= importExecMax {
			keep = append(keep, id)
			continue
		}
		delete(l.byID, id)
	}
	l.order = keep
}

// importExecTerminal 判断执行维是否已终结。
func importExecTerminal(exec string) bool {
	switch exec {
	case protocol.OwnerExecDone, protocol.OwnerExecFailed, protocol.OwnerExecCancelled, protocol.OwnerExecUnknown:
		return true
	default:
		return false
	}
}

// importExecOfPhase 把补报阶段翻译成执行维：completed 才是 done；failed 是 failed；
// outcome_unknown（以及任何没有映射的阶段）是 unknown——不把「没确认」写成 done 或 failed。
func importExecOfPhase(phase string) string {
	switch phase {
	case "completed":
		return protocol.OwnerExecDone
	case "failed":
		return protocol.OwnerExecFailed
	default:
		return protocol.OwnerExecUnknown
	}
}

// OwnerStatus 回报一批导入受理句柄的执行维（S-001 §3.5 的 owner 侧对端）。
//
// recoveryComplete 恒 true：mcp 不持久化异步导入的受理记录，启动后没有待恢复的账本；
// 不认识的 id 就真的没有（可能已超出保留窗口，或服务重启前受理）。
func (s *Server) OwnerStatus(_ context.Context, ids []string) (map[string]string, bool, error) {
	states := make(map[string]string, len(ids))
	for _, id := range ids {
		if exec, ok := s.importExecs.exec(id); ok {
			states[id] = exec
		}
	}
	return states, true, nil
}

// OwnerControl 处理导入句柄的 pause / resume / cancel。
//
// 先认对象（id）再说能力：不认识的句柄回 not_found；认识的句柄上不支持的 op 回
// not_supported 并说明原因——不用「假成功」换确认。
func (s *Server) OwnerControl(_ context.Context, id, op string, _ time.Duration) (string, string, error) {
	if id == "" {
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "mcp.owner_control: id 必填", false, nil)
	}
	exec, known := s.importExecs.exec(id)
	if !known {
		return "", "", spv2.ErrCode(spv2.CodeNotFound,
			fmt.Sprintf("没有这个导入受理句柄（可能已超出 %s 保留窗口，或服务重启前受理）: %s",
				importExecRetention, id), false, nil)
	}
	switch op {
	case protocol.OwnerOpPause, protocol.OwnerOpResume:
		return "", "", spv2.ErrCode(spv2.CodeNotSupported,
			fmt.Sprintf("不支持 %s：mcp 的导入没有暂停/恢复路径（只能取消本地等待，且远端是否停止未知）: %s", op, id),
			false, nil)
	case protocol.OwnerOpCancel:
		return s.cancelImportOwner(id, exec)
	default:
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "未知 op "+op+": "+id, false, nil)
	}
}

// cancelImportOwner 只撤销本进程内的本地等待，不声称远端已停。
//
// 已结束的句柄没有可取消的等待：如实回它的终态（exec 是 done/failed 等未确认值时，
// 调用方按协议视为「未确认取消」），reason 说明为什么。
func (s *Server) cancelImportOwner(id, exec string) (string, string, error) {
	if exec != protocol.OwnerExecRunning {
		return exec, importEndedReason(exec), nil
	}
	if s.asyncImports.cancelByHandle(id) {
		s.importExecs.cancelUnknown(id)
		return protocol.OwnerExecUnknown, importCancelReason, nil
	}
	// 竞态：本地观察刚好在收尾（cancel 已置空但终态还没记账）。以账本里的终态为准；
	// 仍是 running 就如实回「未确认」，不报成功。
	if ended, ok := s.importExecs.exec(id); ok && ended != protocol.OwnerExecRunning {
		return ended, importEndedReason(ended), nil
	}
	return protocol.OwnerExecUnknown, "本地观察正在收尾，取消未生效；远端结果未知", nil
}

// importEndedReason 解释「为什么这条 cancel 没有新的取消效果」。
func importEndedReason(exec string) string {
	switch exec {
	case protocol.OwnerExecCancelled:
		return importCancelReason + "：本地等待此前已撤销，远端工具是否停止未知"
	case protocol.OwnerExecUnknown:
		return "导入已结束但远端结果未知（outcome_unknown），本地没有可取消的等待"
	default:
		return fmt.Sprintf("导入已结束（%s），本地没有可取消的等待", exec)
	}
}
