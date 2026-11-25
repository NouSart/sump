package server

// owner 侧的对账 / 控制面（R-029）：owner_status / owner_control 的服务实现。
//
// timer 的能力边界：
//   - status：在计时器里的句柄是 running；已到点 / 已撤销 / 重启前受理的不认识；
//   - cancel：复用现有撤销路径（摘登记 + 照旧给原调用方发一条 final 补报）；
//   - pause / resume：计时没有暂停概念（暂停等于取消），回 not_supported。
//
// recoveryComplete 恒 true：计时只在内存里（进程重启即丢，见 server.go 文件头），启动后
// 没有待恢复的账本；不认识的句柄意味着它再也不会到点——memory 据此判 orphaned 是对的。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// OwnerStatus 回报一批计时句柄的执行维（在计时器里 = running）。
func (s *Server) OwnerStatus(_ context.Context, ids []string) (map[string]string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	states := make(map[string]string, len(ids))
	for _, id := range ids {
		if _, ok := s.pending[id]; ok {
			states[id] = protocol.OwnerExecRunning
		}
	}
	return states, true, nil
}

// OwnerControl 处理 pause / resume / cancel：只有 cancel 有真实路径（复用 cancelTiming）。
//
// 先认对象（id）再说能力：不认识的句柄回 not_found；认识的句柄上不支持的 op 回
// not_supported 并说明原因——不用"假成功"换确认。
func (s *Server) OwnerControl(_ context.Context, id, op string, _ time.Duration) (string, string, error) {
	s.mu.Lock()
	_, known := s.pending[id]
	s.mu.Unlock()
	if !known {
		return "", "", spv2.ErrCode(spv2.CodeNotFound,
			"没有这条在计时器（可能已到点、已撤销，或服务重启前受理）: "+id, false, nil)
	}
	switch op {
	case protocol.OwnerOpPause, protocol.OwnerOpResume:
		return "", "", spv2.ErrCode(spv2.CodeNotSupported,
			fmt.Sprintf("不支持 %s：计时没有暂停/恢复的概念（暂停等于取消），只能 cancel: %s", op, id),
			false, nil)
	case protocol.OwnerOpCancel:
		out, err := s.cancelTiming(id)
		if err != nil {
			// 只有空 id 会走到这里；这里仍带上 op / id，保持错误信息可定位。
			return "", "", spv2.ErrCode(spv2.CodeInvalidArgument,
				fmt.Sprintf("cancel %s 失败: %v", id, err), false, nil)
		}
		res, ok := out.(protocol.TimerCancelResult)
		if !ok || res.Status != "canceled" {
			// 认对象与撤销之间到点了（或已被撤销）：如实回 not_found，不谎称取消成功。
			return "", "", spv2.ErrCode(spv2.CodeNotFound,
				"没有这条在计时器（可能已到点、已撤销，或服务重启前受理）: "+id, false, nil)
		}
		return protocol.OwnerExecCancelled, "", nil
	default:
		return "", "", spv2.ErrCode(spv2.CodeInvalidArgument, "未知 op "+op+": "+id, false, nil)
	}
}
