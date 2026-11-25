package session

// 调用账本的原生入口（S-001 §3.0.2 / §3.4 / §3.5 / §3.6）：call_open / call_control /
// call_status / resident_list 的服务卡声明与共享实现。
//
// 与源 af6cfc8:src/memory/server/calls_v2.go 的差异（R-023 P3/P4）：
//   - image_note 不搬（D7：image_index 不在本架构），image 相关行一并剥离；
//   - 授权（R-023 P4）：call_open 走 authorizeTargetWork（帧头 work + 载荷 work + 身份），
//     call_control / call_status 走 authorizeCallOp（按目标 call 的 work 归属，含更严的
//     owner/runtime 档）；resident_list 走 authorizeWork，匿名 from.id 一律拒绝；
//   - call_status 单次读（不建周期 reconciler，D7）：未注入 owner 时 owner_recovery_complete
//     恒 false；注入后按 owner service 分批问，问不到不算 orphaned（§3.5 规则 1：
//     问不到 ≠ 不存在）；本地查不到的 id 只回 id；
//   - origin_session 留空：不透明会话 ID 由 experience/redact 提供，session 不能 import
//     experience（架构测试钉死），本笔不在这里复制第二套脱敏实现。
//
// pause / resume 需要 owner 确认（见 owner_client.go）：未注入客户端时明确回 unavailable
// 且不改状态；注入后以 owner 的回答为准（空串 = 已确认但未带状态维）。cancel 始终可本地
// 执行；可暂停调用注入客户端后会先问 owner，未确认时标 remote_may_continue。
// 任何情况下都不得静默写成 paused / running / cancelled 之类的假状态。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// registerCallsV2 注册调用账本 / 常驻目录的原生 method（都不是工具）。
// Name 一律写成字面量：provides 门禁按字面量比对「名片承诺的动作」与「真的注册了」。
func (s *Server) registerCallsV2() {
	s.HandleNative(callOpenMethod(), s.nativeCallOpen)
	s.HandleNative(callControlMethod(), s.nativeCallControl)
	s.HandleNative(callStatusMethod(), s.nativeCallStatus)
	s.HandleNative(residentListMethod(), s.nativeResidentList)
}

// callOpenMethod 是 call_open 的服务卡声明。
func callOpenMethod() spv2.Method {
	return spv2.Method{
		Name:        "call_open",
		Description: "派发前分配 call_id（本地意图：observe=waiting、exec=created）",
		Input: objectSchema(map[string]any{
			"work":             stringSchema(),
			"service":          stringSchema(),
			"action":           stringSchema(),
			"mode":             stringSchema(),
			"pausable":         booleanSchema(),
			"ttl_ms":           integerSchema(),
			"exec_deadline_ms": integerSchema(),
		}, "work", "service"),
		Output: objectSchema(map[string]any{"call_id": stringSchema()}, "call_id"),
	}
}

// callControlMethod 是 call_control 的服务卡声明。
func callControlMethod() spv2.Method {
	return spv2.Method{
		Name:        "call_control",
		Description: "控制一次调用（cancel 可本地执行；pause/resume 需 owner 确认，未接入 owner 客户端时回 unavailable）",
		Input: objectSchema(map[string]any{
			"id":         stringSchema(),
			"control_id": stringSchema(),
			"op":         stringSchema(),
			"ttl_ms":     integerSchema(),
		}, "id", "control_id", "op"),
		Output: objectSchema(map[string]any{
			"id":         stringSchema(),
			"control_id": stringSchema(),
			"observe":    stringSchema(),
			"exec":       stringSchema(),
			"reason":     stringSchema(),
		}, "id", "control_id", "observe", "exec"),
	}
}

// callStatusMethod 是 call_status 的服务卡声明。
func callStatusMethod() spv2.Method {
	return spv2.Method{
		Name:        "call_status",
		Description: "按 ids 读调用状态（对账；未接入 owner 客户端时 owner_recovery_complete 恒 false，不判 orphaned）",
		Input: objectSchema(map[string]any{
			"ids": map[string]any{"type": "array", "items": stringSchema()},
		}, "ids"),
		Output: objectSchema(map[string]any{
			"owner_recovery_complete": booleanSchema(),
			"items":                   map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		}, "owner_recovery_complete", "items"),
	}
}

// residentListMethod 是 resident_list 的服务卡声明。
func residentListMethod() spv2.Method {
	return spv2.Method{
		Name:        "resident_list",
		Description: "列当前 owner 的常驻调用（按 work 解析 owner；owner 不同不可见）",
		Input: objectSchema(map[string]any{
			"work":   stringSchema(),
			"limit":  integerSchema(),
			"cursor": stringSchema(),
		}),
		Output: objectSchema(map[string]any{
			"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"next":  stringSchema(),
		}, "items"),
	}
}

// ---------- 请求结构（线路键名即契约） ----------

// residentListRequest 是 resident_list 的入参。
//
// protocol.ResidentListPayload 里没有 work 字段（协议只承诺 limit + 不透明游标），
// 而常驻目录要按调用方 work 解析 owner（S-001 §9），所以这里用本地结构接收可选的 work：
// 缺省时回退到帧头 frame.work。
type residentListRequest struct {
	Work   string `msgpack:"work,omitempty" json:"work,omitempty"`
	Limit  int    `msgpack:"limit,omitempty" json:"limit,omitempty"`
	Cursor string `msgpack:"cursor,omitempty" json:"cursor,omitempty"`
}

// ---------- 错误翻译 ----------

func callsError(code, msg string) error {
	return spv2.ErrCode(code, msg, spv2.RetryableByCode(code), nil)
}

// mapCallError 把 store 的哨兵错误翻成协议错误码；已是 *spv2.Error 的原样返回。
func mapCallError(err error) error {
	if err == nil {
		return nil
	}
	var e *spv2.Error
	if errors.As(err, &e) {
		return err
	}
	switch {
	case errors.Is(err, store.ErrCallConflict):
		return callsError(spv2.CodeConflict, err.Error())
	case errors.Is(err, store.ErrCallNotFound), errors.Is(err, store.ErrWorkNotFound):
		return callsError(spv2.CodeNotFound, err.Error())
	case errors.Is(err, store.ErrControlNotFound):
		return callsError(spv2.CodeConflict, err.Error())
	default:
		return callsError(spv2.CodeInternal, err.Error())
	}
}

func replyCallsError(call *spv2.Call, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return call.ReplyError(e.Code, e.Message, e.Retryable, e.Details)
	}
	return call.ReplyError(spv2.CodeInternal, err.Error(), true, nil)
}

// isTerminalExec 判断执行维是否已经结束（终态不允许再 cancel）。
func isTerminalExec(exec string) bool {
	switch exec {
	case store.CallExecDone, store.CallExecFailed, store.CallExecCancelled:
		return true
	default:
		return false
	}
}

// ---------- 逻辑层（单测直接调；handler 只做解码 / 回包） ----------

// openCall 校验参数后落一条本地意图（S-001 §3.0.2）。
//
// task / owner 不需要调用方声明：它们由 works 行联表解析（store.CreateCall 只查 work 存在）。
// 旧实现的「work 关联 task 是否存在」检查是给 image_note 解析会话用的，本架构没有这条链路，
// 也就没有可验证的入口，不搬。
func (s *Server) openCall(ctx context.Context, body []byte, c caller) (protocol.CallOpenResult, error) {
	var req protocol.CallOpenPayload
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return protocol.CallOpenResult{}, callsError(spv2.CodeInvalidArgument, "call_open body 无效: "+err.Error())
	}
	work := strings.TrimSpace(req.Work)
	service := strings.TrimSpace(req.Service)
	if work == "" {
		return protocol.CallOpenResult{}, callsError(spv2.CodeInvalidArgument, "call_open: work 必填")
	}
	if service == "" {
		return protocol.CallOpenResult{}, callsError(spv2.CodeInvalidArgument, "call_open: service 必填")
	}
	if req.TTLMS < 0 || req.ExecDeadlineMS < 0 {
		return protocol.CallOpenResult{}, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_open: ttl_ms / exec_deadline_ms 不能为负（收到 %d / %d）", req.TTLMS, req.ExecDeadlineMS))
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = store.CallModeOnce
	}
	if mode != store.CallModeOnce && mode != store.CallModeResident {
		return protocol.CallOpenResult{}, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_open: mode %q 非法（once|resident）", mode))
	}

	// 授权先于落库：跨 work / 匿名请求不产生 call 行。
	if _, err := s.authorizeTargetWork(ctx, work, c.work, "call_open", c.from); err != nil {
		return protocol.CallOpenResult{}, err
	}

	callID, err := s.cfg.Store.CreateCall(ctx, work, service, strings.TrimSpace(req.Action),
		mode, req.Pausable, req.TTLMS, req.ExecDeadlineMS)
	if err != nil {
		return protocol.CallOpenResult{}, mapCallError(err)
	}
	if s.logger != nil {
		s.logger.Printf("call_open: %s (%s/%s, mode=%s)", callID, service, req.Action, mode)
	}
	return protocol.CallOpenResult{CallID: callID}, nil
}

// controlCall 执行 call_control（S-001 §3.4）。
//
// 幂等：同 control_id 重放直接返回首次结果（call_controls 表）；结果还没落库时回 conflict。
// 归属：先按目标 call 的 work + 帧头 from 授权（authorizeCallOp），且授权在读控制记录之前，
// 避免通过 control_id 探知别的 work 的调用细节。
// 控制语义：未注入 owner 客户端时 pause / resume 回 unavailable 且不改状态；cancel 可本地执行
// （可暂停 → cancelled；不可暂停 → unknown + remote_may_continue）。
func (s *Server) controlCall(ctx context.Context, body []byte, c caller) (protocol.CallControlResult, error) {
	var req protocol.CallControlPayload
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return protocol.CallControlResult{}, callsError(spv2.CodeInvalidArgument, "call_control body 无效: "+err.Error())
	}
	id := strings.TrimSpace(req.ID)
	controlID := strings.TrimSpace(req.ControlID)
	op := strings.ToLower(strings.TrimSpace(req.Op))
	if id == "" || controlID == "" || op == "" {
		return protocol.CallControlResult{}, callsError(spv2.CodeInvalidArgument, "call_control: id / control_id / op 必填")
	}
	switch op {
	case "pause", "resume", "cancel":
	default:
		return protocol.CallControlResult{}, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_control: op %q 非法（pause|resume|cancel）", op))
	}
	if c.anonymous() {
		return protocol.CallControlResult{}, authDenied("call_control", "调用方身份为空，拒绝操作")
	}

	callRow, err := s.cfg.Store.GetCall(ctx, id)
	if err != nil {
		return protocol.CallControlResult{}, mapCallError(err)
	}
	// 跨 work 的 id（含 control_id 重放路径）一律拒绝，且拒绝发生在读控制记录之前。
	if err := s.authorizeCallOp(ctx, callRow, c.work, c.from, "call_control"); err != nil {
		return protocol.CallControlResult{}, err
	}

	// 幂等重放：control_id 已存在时只看首次结果，不再执行。
	if existing, ok, err := s.cfg.Store.GetControl(ctx, controlID); err != nil {
		return protocol.CallControlResult{}, mapCallError(err)
	} else if ok {
		if existing.CallID != id || existing.Op != op {
			// 不回显别的调用的 id / op：control_id 是调用方给的，不能借冲突错误泄漏他人参数。
			return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: control_id %s 已被其它调用或操作使用", controlID))
		}
		if existing.Result == "" {
			return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: control_id %s 的首次结果尚未落库，不能重放", controlID))
		}
		first, err := store.DecodeControlResult(existing.Result)
		if err != nil {
			return protocol.CallControlResult{}, mapCallError(err)
		}
		return protocol.CallControlResult{ID: id, ControlID: controlID,
			Observe: first.Observe, Exec: first.Exec, Reason: first.Reason}, nil
	}

	if callRow.Observe == store.CallObserveClosed {
		return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
			fmt.Sprintf("call_control: call %s 已关闭", id))
	}

	switch op {
	case "pause":
		if !callRow.Pausable {
			return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: call %s 不可暂停（pausable=false）", id))
		}
		return s.pauseCall(ctx, controlID, callRow, req.TTLMS)
	case "resume":
		if callRow.Exec != store.CallExecPaused {
			return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: resume 只能从 paused → running（当前 exec=%s）", callRow.Exec))
		}
		return s.resumeCall(ctx, controlID, callRow, req.TTLMS)
	case "cancel":
		if isTerminalExec(callRow.Exec) {
			return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: call %s 已结束（exec=%s），不能再取消", id, callRow.Exec))
		}
		return s.cancelCall(ctx, controlID, callRow, req.TTLMS)
	}
	return protocol.CallControlResult{}, callsError(spv2.CodeInternal, "call_control: 未处理的分支 "+op)
}

// pauseCall 向 owner 发起暂停握手（S-001 §3.4 / §5.1）。
//
// 只有 owner 明确回答 paused（或空串确认）才写 paused。报错 / 超时 / 非预期回答都只把
// 观察维置为 not_waiting 并记 pause_failed：执行维与 phase 保持原值——ACK 丢失时
// owner 可能其实已经暂停，但 session 不能据此写 paused，更不能写终态 phase。
func (s *Server) pauseCall(ctx context.Context, controlID string, row store.CallRow, ttlMS int64) (protocol.CallControlResult, error) {
	client := s.owner()
	if client == nil {
		// 没有 owner：没有谁能确认暂停，不能本地假装成功（否则会误导调用方与恢复逻辑）。
		return protocol.CallControlResult{}, callsError(spv2.CodeUnavailable,
			"call_control: pause 需要 owner 确认；当前未注入执行服务客户端，不能标记 paused")
	}
	// pause 只对「已受理」的调用有意义：created 是本地意图，还没派发（§3.0.2）。
	if row.Exec != store.CallExecRunning && row.Exec != store.CallExecUnknown {
		return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
			fmt.Sprintf("call_control: pause 只能作用于已受理的调用（当前 exec=%s）", row.Exec))
	}
	if strings.TrimSpace(row.RemoteItemID) == "" {
		return protocol.CallControlResult{}, callsError(spv2.CodeUnavailable,
			fmt.Sprintf("call_control: pause %s 没有 owner 受理句柄（remote_item_id 未回填），无法向 owner 确认", row.ID))
	}
	cctx, cancel := ownerControlContext(ctx, ttlMS)
	defer cancel()
	confirmedExec, err := client.Pause(cctx, row.Service, row.RemoteItemID)
	if err != nil || !ownerConfirmed(confirmedExec, store.CallExecPaused) {
		if s.logger != nil {
			s.logger.Printf("call_control: pause %s 未获 owner 确认（err=%v exec=%q），保持 exec=%s",
				row.ID, err, confirmedExec, row.Exec)
		}
		if err := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveNotWaiting,
			row.Exec, row.Phase, "pause_failed"); err != nil {
			return protocol.CallControlResult{}, mapCallError(err)
		}
		return s.finishControl(ctx, controlID, row.ID, "pause", protocol.CallControlResult{
			ID: row.ID, ControlID: controlID,
			Observe: store.CallObserveNotWaiting, Exec: row.Exec, Reason: "pause_failed",
		})
	}
	if err := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveNotWaiting,
		store.CallExecPaused, row.Phase, "pause_confirmed"); err != nil {
		return protocol.CallControlResult{}, mapCallError(err)
	}
	return s.finishControl(ctx, controlID, row.ID, "pause", protocol.CallControlResult{
		ID: row.ID, ControlID: controlID,
		Observe: store.CallObserveNotWaiting, Exec: store.CallExecPaused, Reason: "pause_confirmed",
	})
}

// resumeCall 向 owner 发起恢复握手（S-001 §3.4）。
// 成功（owner 确认在跑）才写 running + waiting；失败保持原状态，只记 resume_failed。
func (s *Server) resumeCall(ctx context.Context, controlID string, row store.CallRow, ttlMS int64) (protocol.CallControlResult, error) {
	client := s.owner()
	if client == nil {
		return protocol.CallControlResult{}, callsError(spv2.CodeUnavailable,
			"call_control: resume 需要 owner 确认；当前未注入执行服务客户端，不能标记 running")
	}
	if strings.TrimSpace(row.RemoteItemID) == "" {
		return protocol.CallControlResult{}, callsError(spv2.CodeUnavailable,
			fmt.Sprintf("call_control: resume %s 没有 owner 受理句柄（remote_item_id 未回填），无法向 owner 确认", row.ID))
	}
	cctx, cancel := ownerControlContext(ctx, ttlMS)
	defer cancel()
	confirmedExec, err := client.Resume(cctx, row.Service, row.RemoteItemID)
	if err != nil || !ownerConfirmed(confirmedExec, store.CallExecRunning) {
		if s.logger != nil {
			s.logger.Printf("call_control: resume %s 未获 owner 确认（err=%v exec=%q），保持 exec=%s",
				row.ID, err, confirmedExec, row.Exec)
		}
		// 保持原状态（paused），只更新原因：恢复失败不能假装在跑。
		if err := s.cfg.Store.SetCallState(ctx, row.ID, row.Observe,
			row.Exec, row.Phase, "resume_failed"); err != nil {
			return protocol.CallControlResult{}, mapCallError(err)
		}
		return s.finishControl(ctx, controlID, row.ID, "resume", protocol.CallControlResult{
			ID: row.ID, ControlID: controlID,
			Observe: row.Observe, Exec: row.Exec, Reason: "resume_failed",
		})
	}
	if err := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveWaiting,
		store.CallExecRunning, row.Phase, "resume_confirmed"); err != nil {
		return protocol.CallControlResult{}, mapCallError(err)
	}
	return s.finishControl(ctx, controlID, row.ID, "resume", protocol.CallControlResult{
		ID: row.ID, ControlID: controlID,
		Observe: store.CallObserveWaiting, Exec: store.CallExecRunning, Reason: "resume_confirmed",
	})
}

// cancelCall 执行取消（S-001 §3.4）。
//
// 观察维一律 closed（本地不再等）；执行维分四种：
//   - 未注入 owner（本地语义）：可暂停 → cancelled；不可暂停 → unknown + remote_may_continue；
//   - 注入 owner 且不可暂停：不发控制（发了也停不下来），closed/unknown + remote_may_continue，
//     reason=cancelled_local；
//   - 注入 owner 且可暂停、但没有受理句柄（remote_item_id 未回填）：没有对象可问，
//     不假发控制帧，closed/unknown + remote_may_continue，reason=cancel_unconfirmed；
//   - 注入 owner 且可暂停、有受理句柄：问 owner；确认 → closed/cancelled；未确认（报错/超时/ACK 丢失/回答还在跑）→
//     closed/unknown + remote_may_continue，reason=cancel_unconfirmed，不写 phase。
func (s *Server) cancelCall(ctx context.Context, controlID string, row store.CallRow, ttlMS int64) (protocol.CallControlResult, error) {
	var exec, phase, reason string
	remoteMayContinue := false

	switch {
	case s.owner() == nil:
		// 没有 owner 可问：保留本地取消语义。
		if row.Pausable {
			exec, phase, reason = store.CallExecCancelled, store.CallPhaseCancelled, "cancelled"
		} else {
			exec, phase, reason = store.CallExecUnknown, "", "remote_may_continue"
			remoteMayContinue = true
		}
	case !row.Pausable:
		// 不可暂停调用：取消只能保证 session 不再等，远端可能还在继续（S-001 §3.4）。
		exec, phase, reason = store.CallExecUnknown, "", "cancelled_local"
		remoteMayContinue = true
	case strings.TrimSpace(row.RemoteItemID) == "":
		// 可暂停但还没回填受理句柄：也许已派发、只是回填丢了；不能凭「本地没有句柄」
		// 断言远端已停，也不猜一个 id 去发控制帧。
		exec, phase, reason = store.CallExecUnknown, "", "cancel_unconfirmed"
		remoteMayContinue = true
	default:
		cctx, cancel := ownerControlContext(ctx, ttlMS)
		defer cancel()
		confirmedExec, err := s.owner().Cancel(cctx, row.Service, row.RemoteItemID)
		if err != nil || !ownerConfirmed(confirmedExec, store.CallExecCancelled) {
			// 取消未获确认：不能假装执行侧已停，也不写终态 phase。
			// owner 明确回答「还在跑」（exec=running）同样是未确认——把它记成 cancelled 是造假。
			if s.logger != nil {
				s.logger.Printf("call_control: cancel %s 未获 owner 确认（err=%v exec=%q），标 remote_may_continue",
					row.ID, err, confirmedExec)
			}
			exec, phase, reason = store.CallExecUnknown, "", "cancel_unconfirmed"
			remoteMayContinue = true
		} else {
			exec, phase, reason = store.CallExecCancelled, store.CallPhaseCancelled, "cancelled"
		}
	}

	if err := s.cfg.Store.SetCallState(ctx, row.ID, store.CallObserveClosed, exec, phase, reason); err != nil {
		return protocol.CallControlResult{}, mapCallError(err)
	}
	if remoteMayContinue {
		if err := s.cfg.Store.SetCallRemoteMayContinue(ctx, row.ID, true); err != nil {
			return protocol.CallControlResult{}, mapCallError(err)
		}
	}
	return s.finishControl(ctx, controlID, row.ID, "cancel", protocol.CallControlResult{
		ID: row.ID, ControlID: controlID, Observe: store.CallObserveClosed, Exec: exec, Reason: reason,
	})
}

// finishControl 落 control_id 的首次回执（幂等记录）并返回控制结果。
func (s *Server) finishControl(ctx context.Context, controlID, callID, op string, res protocol.CallControlResult) (protocol.CallControlResult, error) {
	if err := s.recordControl(ctx, controlID, callID, op, res); err != nil {
		return protocol.CallControlResult{}, mapCallError(err)
	}
	return res, nil
}

// recordControl 落 control_id 的首次回执（幂等；并发下保留先写入的结果）。
func (s *Server) recordControl(ctx context.Context, controlID, callID, op string, res protocol.CallControlResult) error {
	_, deduped, err := s.cfg.Store.PutControl(ctx, controlID, callID, op)
	if err != nil {
		return err
	}
	if deduped {
		// 并发路径：另一个请求已经记了首次结果；保留它，不覆盖。
		return nil
	}
	return s.cfg.Store.SetControlResult(ctx, controlID, res.Observe, res.Exec, res.Reason)
}

// callStatus 按 ids 读调用状态（对账读取；S-001 §3.5 / §5.4）。
//
// 未注入 OwnerClient 时 owner_recovery_complete 恒 false，items 只回本地账本；
// 注入后按 owner service 分批询问，**问的是 calls.remote_item_id（owner 的受理句柄），
// 不是 memory 的 call_id**：
//   - owner 报错 / 超时（离线）：整体 recovery_complete=false，本地观测原样返回，
//     **不**把「问不到」判 orphaned（§3.5 规则 1）；
//   - owner 恢复完整（recoveryComplete=true）但明确不认识某个本地已知 call：
//     执行真相丢失（§5.4 灾难路径），报告 exec=unknown、phase=orphaned；
//   - 还没有受理句柄的 call（remote_item_id 为空，多半是 created）：没有对象可问，
//     保持本地观测，**不**判 orphaned——「没回填句柄」不是「owner 不认识」；
//   - 本地账本完全查不到的 id：没有 service 可问，保持 unknown（不回 exec/phase，
//     更不判 orphaned）——这是「问不到 ≠ 不存在」的硬边界；
//   - owner 认识的 call 按其返回的 exec 上报（只读对账，本地账本不动）。
//
// 单次读、不替 owner 推进状态：这里只回答"现在是什么"；周期对账在 reconcile.go。
// 归属：与 call_control 同口径（authorizeCallOp）逐 id 校验；任一本地已知 id
// 跨 work / 身份不符即整体 permission_denied，不通过 items 泄漏别的 work 的调用。
func (s *Server) callStatus(ctx context.Context, body []byte, c caller) (protocol.CallStatusResult, error) {
	var req protocol.CallStatusPayload
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return protocol.CallStatusResult{}, callsError(spv2.CodeInvalidArgument, "call_status body 无效: "+err.Error())
	}
	out := protocol.CallStatusResult{OwnerRecoveryComplete: false, Items: []protocol.CallStatusItem{}}
	if len(req.IDs) == 0 {
		return out, callsError(spv2.CodeInvalidArgument, "call_status: ids 不能为空")
	}
	if len(req.IDs) > 100 {
		return out, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_status: 单批最多 100 个 id（收到 %d）", len(req.IDs)))
	}
	if c.anonymous() {
		return out, authDenied("call_status", "调用方身份为空，拒绝操作")
	}

	// 先按本地账本把 id 归到 owner service：calls 表有 service 列，本地没有的 id
	// 没有可问的对象（owner 归属由 works 解析，不是靠猜）。
	type statusEntry struct {
		id  string
		row store.CallRow
		ok  bool
	}
	entries := make([]statusEntry, 0, len(req.IDs))
	byService := map[string][]string{}
	for _, raw := range req.IDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		row, err := s.cfg.Store.GetCall(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrCallNotFound) {
				// 查不到不等于 orphaned：没有 owner 可问（S-001 §3.5 规则 1）。
				entries = append(entries, statusEntry{id: id})
				continue
			}
			return out, mapCallError(err)
		}
		// 归属校验：跨 work 的已知 id 直接拒绝（S-001 §9；R-023 P4）。
		if err := s.authorizeCallOp(ctx, row, c.work, c.from, "call_status"); err != nil {
			return out, err
		}
		entries = append(entries, statusEntry{id: id, row: row, ok: true})
		// 只把有受理句柄的 call 交给 owner：句柄才是 owner 认识的 id。
		if row.Service != "" && strings.TrimSpace(row.RemoteItemID) != "" {
			byService[row.Service] = append(byService[row.Service], row.RemoteItemID)
		}
	}

	client := s.owner()
	if client == nil || len(byService) == 0 {
		// 未接 owner，或没有任何「本地已知 call」可提供 owner service：
		// 只能回本地账本，且 recovery_complete 保持 false（没有依据说恢复完整）。
		for _, e := range entries {
			if e.ok {
				out.Items = append(out.Items, callStatusItemFor(e.row))
			} else {
				out.Items = append(out.Items, protocol.CallStatusItem{ID: e.id})
			}
		}
		return out, nil
	}

	// 按 owner service 分批问；任一 service 未确认就不把「不认识」当 orphaned（保守取 AND）。
	states := map[string]string{}
	recoveryComplete := true
	for service, ids := range byService {
		cctx, cancel := ownerStatusContext(ctx)
		got, complete, err := client.Status(cctx, service, ids)
		cancel()
		if err != nil {
			// owner 离线 / 超时：这一批不可判定；本地观测原样返回，不产生 orphaned。
			recoveryComplete = false
			if s.logger != nil {
				s.logger.Printf("call_status: owner %s 对账失败（%v），保持本地观测", service, err)
			}
			continue
		}
		if !complete {
			recoveryComplete = false
		}
		for id, exec := range got {
			states[id] = exec
		}
	}

	for _, e := range entries {
		if !e.ok {
			// 本地未知的 id：没有 owner 可问，保持 unknown（不写 exec / phase）。
			out.Items = append(out.Items, protocol.CallStatusItem{ID: e.id})
			continue
		}
		item := callStatusItemFor(e.row)
		remoteID := strings.TrimSpace(e.row.RemoteItemID)
		if exec, known := states[remoteID]; known && remoteID != "" && strings.TrimSpace(exec) != "" {
			item.Exec = exec
		} else if remoteID != "" && recoveryComplete {
			// owner 恢复完整、有受理句柄、却明确不认识：执行真相丢失（§5.4 灾难路径）。
			// 只在报告层标 orphaned；本地账本不动。
			item.Exec = store.CallExecUnknown
			item.Phase = store.CallPhaseOrphaned
		}
		// 没有受理句柄的 call：没有对象可问，保持本地观测，不判 orphaned。
		out.Items = append(out.Items, item)
	}
	out.OwnerRecoveryComplete = recoveryComplete
	return out, nil
}

// callStatusItemFor 把本地 call 行投影成对账项（owner 的答案覆盖 exec 之前在调用方处做）。
func callStatusItemFor(row store.CallRow) protocol.CallStatusItem {
	return protocol.CallStatusItem{
		ID:           row.ID,
		Observe:      row.Observe,
		Exec:         row.Exec,
		Phase:        row.Phase,
		OwnerService: row.Service,
		UpdatedAt:    row.UpdatedAt,
	}
}

// residentListFor 是 resident_list 的实现（handler 解码后直接调；单测也可直接调）。
// owner 来自载荷 work（缺省 frame.work）对应的 works 行；解析不到 work 即 not_found。
// 授权与源实现同口径（authorizeWork）：owner/runtime 可跨 work 查看，普通调用方必须是
// 该 work 的 agent/owner/entry；匿名 from.id 一律拒绝（不因为 authority=owner 放行）。
func (s *Server) residentListFor(ctx context.Context, req residentListRequest, c caller) (protocol.ResidentListResult, error) {
	out := protocol.ResidentListResult{Items: []protocol.ResidentItem{}}
	work := strings.TrimSpace(req.Work)
	if work == "" {
		return out, callsError(spv2.CodeInvalidArgument,
			"resident_list: work 必填（payload.work 与 frame.work 都为空，无法解析常驻目录归属）")
	}
	if c.anonymous() {
		return out, authDenied("resident_list", "调用方身份为空，拒绝操作")
	}
	w, err := s.cfg.Store.GetWork(ctx, work)
	if err != nil {
		return out, mapCallError(err)
	}
	if err := authorizeWork(w, c.from); err != nil {
		return out, authDenied("resident_list", "无权查看该 work 的常驻目录")
	}
	owner := strings.TrimSpace(w.Owner.ID)
	if owner == "" {
		return out, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("resident_list: work %s 没有 owner，无法解析常驻目录归属", work))
	}
	limit := req.Limit
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		limit = 200
	}
	rows, err := s.cfg.Store.ListResident(ctx, owner, limit, req.Cursor)
	if err != nil {
		return out, mapCallError(err)
	}
	for _, row := range rows {
		title := row.Action
		if title == "" {
			title = row.Service
		}
		out.Items = append(out.Items, protocol.ResidentItem{
			ID:           row.ID,
			Kind:         "call",
			Title:        title,
			Observe:      row.Observe,
			Exec:         row.Exec,
			Phase:        row.Phase,
			OwnerService: row.Service,
			// OriginSession 留空：不透明会话 ID 在 experience/redact，session 不得 import
			// experience（架构测试）；这里不复制第二套脱敏实现。
			CreatedAt: row.CreatedAt,
			UpdatedAt: row.UpdatedAt,
		})
	}
	if len(rows) == limit {
		out.Next = rows[len(rows)-1].ID
	}
	return out, nil
}

// ---------- 原生 handler（解码 / 回包） ----------

func (s *Server) nativeCallOpen(ctx context.Context, call *spv2.Call) error {
	out, err := s.openCall(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeCallControl(ctx context.Context, call *spv2.Call) error {
	out, err := s.controlCall(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeCallStatus(ctx context.Context, call *spv2.Call) error {
	out, err := s.callStatus(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeResidentList(ctx context.Context, call *spv2.Call) error {
	var req residentListRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "resident_list body 无效: "+err.Error(), false, nil)
	}
	// payload 没带 work 时回退到帧头 work（调用方 work）。
	if strings.TrimSpace(req.Work) == "" {
		req.Work = call.WorkID()
	}
	out, err := s.residentListFor(ctx, req, callerOf(call))
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}
