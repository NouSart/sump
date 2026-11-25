package session

// 周期 call 对账（S-001 §3.5 / R-029-D4）：只读地追问 owner，把执行维与 phase 保守地
// 合并回本地账本。**不发控制、不重试有副作用的操作**——对账只回答「现在是什么」。
//
// 映射：calls.remote_item_id → owner_status.states 的键（owner 自己的受理句柄）。
// 没有受理句柄的 call（多半是 created）不发上线：没回填句柄 ≠ owner 不认识。
//
// 规则：
//   - recovery_complete=false（owner 启动早期）不判 orphaned——空结果会误杀（§3.5 规则 1）；
//   - owner 恢复完整、有句柄、明确不认识（states 里没有）且本地非 created：
//     exec=unknown、phase=orphaned、observe=not_waiting（§5.4 灾难路径）；created 交给到期规则；
//   - created 且过 exec_deadline_ms：exec=unknown、observe=not_waiting、不写 phase、
//     reason=dispatch_unconfirmed（**不转 cancelled**：派发确认不了，不等于取消成功）；
//   - owner 报出的执行维按现有两维状态机（SetCallState 里的 canExec/canObserve）保守合并，
//     非法转移只记日志、不绕状态机（created → 终态要先落一跳 running：owner 的回答就是
//     受理证据，状态机本身允许 created→running）；observe 只在状态机要求的方向上改
//     （终态 → closed、不可判定 → not_waiting），不重新打开已关闭的 call；
//   - owner 离线（报错 / 超时 / unknown method）：按 service 指数退避（基 30s、封顶 60s），
//     不把「问不到」写成任何状态。
//
// 指标（只从本服务骨架的 Registry 记）：
//   sump_call_reconcile_scanned_total              本轮扫描的 call 条数
//   sump_call_reconcile_updated_total              本轮真正改写的 call 条数
//   sump_call_reconcile_owner_offline_total{service}  owner 批次失败次数
//
// 默认参数是 S-001 §3.5 的配置值（不是协议不变量）；抽成变量只为测试注入。

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

var (
	// callReconcileInterval 是对账周期（S-001 §3.5 默认 30s）。
	callReconcileInterval = 30 * time.Second
	// ownerReconcileBatchSize 是每次 owner_status 最多带的句柄数（S-001 §3.5 默认 100）。
	ownerReconcileBatchSize = 100
	// ownerReconcileBackoffBase / Cap 是 owner 离线退避的起点与封顶（S-001 §3.5：指数、封顶 60s）。
	ownerReconcileBackoffBase = 30 * time.Second
	ownerReconcileBackoffCap  = 60 * time.Second
)

const (
	reconcileScannedMetric      = "sump_call_reconcile_scanned_total"
	reconcileUpdatedMetric      = "sump_call_reconcile_updated_total"
	reconcileOwnerOfflineMetric = "sump_call_reconcile_owner_offline_total"
)

// ownerReconcileState 是某个 owner service 的离线退避状态。
type ownerReconcileState struct {
	failures    int
	nextAttempt time.Time
}

// reconcileCalls 跑一轮对账（周期循环与测试共用；测试用 reconcileCallsTo 注入 Registry）。
func (s *Server) reconcileCalls(ctx context.Context) {
	s.reconcileCallsTo(ctx, s.Metrics())
}

// reconcileCallsTo 是 reconcileCalls 的可测形态：metrics 可为 nil（未启动的服务骨架）。
func (s *Server) reconcileCallsTo(ctx context.Context, metrics *spv2.Registry) {
	rows, err := s.cfg.Store.ListCallsForReconcile(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("call 对账: 读本地账本失败: %v", err)
		}
		return
	}
	recordReconcileMetric(metrics, reconcileScannedMetric, len(rows), nil)

	byService := map[string][]store.CallRow{}
	var created []store.CallRow
	for _, row := range rows {
		if row.Exec == store.CallExecCreated {
			created = append(created, row)
		}
		if row.Service != "" && strings.TrimSpace(row.RemoteItemID) != "" {
			byService[row.Service] = append(byService[row.Service], row)
		}
	}

	client := s.owner()
	if client != nil {
		services := make([]string, 0, len(byService))
		for service := range byService {
			services = append(services, service)
		}
		sort.Strings(services) // 顺序稳定：日志与指标可复现
		for _, service := range services {
			if s.ownerBackoffActive(service, time.Now()) {
				continue
			}
			if !s.reconcileOwnerRows(ctx, client, service, byService[service], metrics) {
				s.noteOwnerOffline(service, time.Now())
				recordReconcileMetric(metrics, reconcileOwnerOfflineMetric, 1,
					map[string]string{"service": service})
				continue
			}
			s.noteOwnerOnline(service)
		}
	}

	// created 到期最后处理：先让 owner 的回答有机会把它推进到真实状态；
	// 重新读一遍，避免把本轮刚推进的 created 误判成"派发未确认"。
	for _, snap := range created {
		row, err := s.cfg.Store.GetCall(ctx, snap.ID)
		if err != nil || row.Exec != store.CallExecCreated {
			continue
		}
		expired, err := createdExpired(row, time.Now())
		if err != nil {
			if s.logger != nil {
				s.logger.Printf("call 对账: call %s 的 created_at 无法解析，跳过到期判断: %v", row.ID, err)
			}
			continue
		}
		if !expired {
			continue
		}
		if err := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveNotWaiting,
			store.CallExecUnknown, "", "dispatch_unconfirmed"); err != nil {
			if s.logger != nil {
				s.logger.Printf("call 对账: call %s 派发未确认，转 unknown 失败: %v", row.ID, err)
			}
			continue
		}
		recordReconcileMetric(metrics, reconcileUpdatedMetric, 1, nil)
	}
}

// reconcileOwnerRows 按批问一个 owner service，并把答案保守合并回本地账本。
// 返回 false 表示这一轮该 owner 不可用（任一 批失败）：整批不做任何合并，
// 避免用半批答案判 orphaned。
func (s *Server) reconcileOwnerRows(ctx context.Context, client OwnerClient, service string, rows []store.CallRow, metrics *spv2.Registry) bool {
	states := map[string]string{}
	recoveryComplete := true
	for start := 0; start < len(rows); start += ownerReconcileBatchSize {
		end := start + ownerReconcileBatchSize
		if end > len(rows) {
			end = len(rows)
		}
		chunk := rows[start:end]
		ids := make([]string, 0, len(chunk))
		for _, row := range chunk {
			ids = append(ids, row.RemoteItemID)
		}
		cctx, cancel := ownerStatusContext(ctx)
		got, complete, err := client.Status(cctx, service, ids)
		cancel()
		if err != nil {
			if s.logger != nil {
				s.logger.Printf("call 对账: owner %s 不可用（%d 条，第 %d 批）: %v",
					service, len(rows), start/ownerReconcileBatchSize+1, err)
			}
			return false
		}
		recoveryComplete = recoveryComplete && complete
		for id, exec := range got {
			states[id] = exec
		}
	}

	for _, row := range rows {
		exec, known := states[row.RemoteItemID]
		if known {
			s.mergeOwnerExec(ctx, row, exec, metrics)
			continue
		}
		if !recoveryComplete {
			// owner 还没恢复完持久受理记录：「不认识」不能判 orphaned（§3.5 规则 1）。
			continue
		}
		if row.Exec == store.CallExecCreated {
			// created 的生死由到期规则判（见 reconcileCallsTo）：没受理证据不等于丢失。
			continue
		}
		s.mergeOrphan(ctx, row, metrics)
	}
	return true
}

// reconcileTarget 是 owner 答案对应的本地落点。
type reconcileTarget struct {
	observe string
	exec    string
	phase   string
	reason  string
}

// reconcileTargetOf 把 owner_status 的执行维翻成本地落点；空串 / 未定义的取值不猜（ok=false）。
func reconcileTargetOf(ownerExec string) (reconcileTarget, bool) {
	switch ownerExec {
	case protocol.OwnerExecCreated:
		return reconcileTarget{store.CallObserveWaiting, store.CallExecCreated, "", ""}, true
	case protocol.OwnerExecRunning:
		return reconcileTarget{store.CallObserveWaiting, store.CallExecRunning, "", ""}, true
	case protocol.OwnerExecPaused:
		// 暂停确认后本地不再等（与 pauseCall 的落点一致）。
		return reconcileTarget{store.CallObserveNotWaiting, store.CallExecPaused, "", ""}, true
	case protocol.OwnerExecDone:
		return reconcileTarget{store.CallObserveClosed, store.CallExecDone, store.CallPhaseDone, ""}, true
	case protocol.OwnerExecFailed:
		return reconcileTarget{store.CallObserveClosed, store.CallExecFailed, store.CallPhaseFailed, ""}, true
	case protocol.OwnerExecCancelled:
		return reconcileTarget{store.CallObserveClosed, store.CallExecCancelled, store.CallPhaseCancelled, ""}, true
	case protocol.OwnerExecUnknown:
		return reconcileTarget{store.CallObserveNotWaiting, store.CallExecUnknown, "", ""}, true
	default:
		return reconcileTarget{}, false
	}
}

// mergeOwnerExec 把一个 owner 报出的执行维保守合并进本地账本。
//
// 状态机仍是唯一口径：先按目标元组尝试 SetCallState，冲突时不硬写。唯一的前置跳是
// created → running——owner 的答案本身就是「已受理」的证据，而状态机允许这一跳，
// 之后再到终态就合法了（与 applyReportToCall 对「owner 第一次报告就是终态」的处理同源）。
func (s *Server) mergeOwnerExec(ctx context.Context, row store.CallRow, ownerExec string, metrics *spv2.Registry) {
	target, ok := reconcileTargetOf(ownerExec)
	if !ok {
		if s.logger != nil {
			s.logger.Printf("call 对账: call %s 的 owner exec=%q 不认识，保持本地 %s/%s",
				row.ID, ownerExec, row.Exec, row.Phase)
		}
		return
	}
	if ownerExec == protocol.OwnerExecUnknown {
		// owner 自己说「不知道」不是新事实：本地已经 unknown 时不覆盖（尤其别清掉 orphaned）。
		if row.Exec == store.CallExecUnknown {
			return
		}
		// 从已知状态退回 unknown：保留既有 phase（如有），不把 phase 一起抹掉。
		target.phase = row.Phase
	}
	if row.Observe == target.observe && row.Exec == target.exec && row.Phase == target.phase {
		return
	}
	err := s.cfg.Store.SetCallState(ctx, row.ID, target.observe, target.exec, target.phase, target.reason)
	if err != nil && errors.Is(err, store.ErrCallConflict) && row.Exec == store.CallExecCreated {
		// created 是本地意图；owner 已经给出终态答案时，先落一跳 running（合法转移），
		// 再按 owner 的答案合并。
		if perr := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveWaiting,
			store.CallExecRunning, "", ""); perr == nil {
			err = s.cfg.Store.SetCallState(ctx, row.ID, target.observe, target.exec, target.phase, target.reason)
		}
	}
	if err != nil {
		if s.logger != nil {
			s.logger.Printf("call 对账: 合并 call %s → %s/%s 未生效（不绕状态机）: %v",
				row.ID, target.exec, target.phase, err)
		}
		return
	}
	recordReconcileMetric(metrics, reconcileUpdatedMetric, 1, nil)
}

// mergeOrphan 把「owner 恢复完整且明确不认识」的已受理 call 落成 unknown/orphaned（§5.4）。
// 只写执行维 / phase；observe 改为 not_waiting（不再等一个执行真相已经丢失的调用）。
func (s *Server) mergeOrphan(ctx context.Context, row store.CallRow, metrics *spv2.Registry) {
	if row.Exec == store.CallExecUnknown && row.Phase == store.CallPhaseOrphaned {
		return
	}
	if err := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveNotWaiting,
		store.CallExecUnknown, store.CallPhaseOrphaned, "orphaned"); err != nil {
		if s.logger != nil {
			s.logger.Printf("call 对账: call %s（%s/%s）不能判 orphaned，保持本地: %v",
				row.ID, row.Observe, row.Exec, err)
		}
		return
	}
	recordReconcileMetric(metrics, reconcileUpdatedMetric, 1, nil)
}

// createdExpired 判断 created 是否已过 exec_deadline_ms（从本地意图落库时间算）。
// exec_deadline_ms<=0 = 没有到期约束（不据此改状态）。
func createdExpired(row store.CallRow, now time.Time) (bool, error) {
	if row.ExecDeadlineMS <= 0 {
		return false, nil
	}
	created, err := time.Parse(time.RFC3339Nano, row.CreatedAt)
	if err != nil {
		return false, err
	}
	return now.Sub(created) > time.Duration(row.ExecDeadlineMS)*time.Millisecond, nil
}

// ownerReconcileBackoff 返回连续 failures 次失败后的退避时长：基 30s 指数增长、封顶 60s。
func ownerReconcileBackoff(failures int) time.Duration {
	if failures <= 0 {
		return 0
	}
	d := ownerReconcileBackoffBase
	for i := 1; i < failures && d < ownerReconcileBackoffCap; i++ {
		d *= 2
	}
	if d > ownerReconcileBackoffCap {
		d = ownerReconcileBackoffCap
	}
	return d
}

// ownerBackoffActive 判断某个 owner service 是否还在退避窗口内。
func (s *Server) ownerBackoffActive(service string, now time.Time) bool {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	st, ok := s.reconcileOwners[service]
	return ok && now.Before(st.nextAttempt)
}

// noteOwnerOffline 记一次 owner 批次失败，并推迟下次尝试。
func (s *Server) noteOwnerOffline(service string, now time.Time) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	if s.reconcileOwners == nil {
		s.reconcileOwners = map[string]ownerReconcileState{}
	}
	st := s.reconcileOwners[service]
	st.failures++
	st.nextAttempt = now.Add(ownerReconcileBackoff(st.failures))
	s.reconcileOwners[service] = st
}

// noteOwnerOnline 清掉某个 owner service 的退避状态（成功一轮即可恢复周期）。
func (s *Server) noteOwnerOnline(service string) {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	delete(s.reconcileOwners, service)
}

// recordReconcileMetric 是 nil 安全的指标累加（未 Start 的骨架没有 Registry）。
func recordReconcileMetric(m *spv2.Registry, name string, delta int, labels map[string]string) {
	if m == nil || delta == 0 {
		return
	}
	m.Counter(name, reconcileMetricHelp(name), labels, float64(delta))
}

func reconcileMetricHelp(name string) string {
	switch name {
	case reconcileScannedMetric:
		return "周期对账本轮扫描的 call 条数"
	case reconcileUpdatedMetric:
		return "周期对账本轮改写的 call 条数"
	case reconcileOwnerOfflineMetric:
		return "周期对账 owner 批次失败次数"
	default:
		return "周期对账指标"
	}
}
