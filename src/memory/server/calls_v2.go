package server

// 调用账本 / 常驻目录 / 图片索引的原生入口（S-001 §3.3 / §3.4 / §3.5 / §3.6 / §9）。
//
// 注册入口单独抽成 registerCallsV2()：server.New 只负责调用它（接线由主代理完成），
// 与既有动作互不纠缠；测试也能单独校验 method 清单与逻辑层。
//
// P1 的诚实边界（与 store/calls.go 文件头一致）：
//   - pause / resume 需要 owner 确认：未注入 OwnerClient 时这里明确返回 unavailable，
//     **不**把状态改成 paused / running；注入后以 owner 的确认/答案为准（见 owner_client.go）。
//   - call_status 在未注入 OwnerClient 时 owner_recovery_complete 恒为 false：不能把
//     “不认识这个 call”判成 orphaned（S-001 §3.5 规则 1）；注入后按 owner 的回答上报。
//   - cancel 始终可以本地执行；可暂停调用注入 client 后会先问 owner，未确认时标
//     remote_may_continue（S-001 §3.4）。
//   - calls 表没有 task / owner / instance 列：owner / task 从 works 行解析，
//     instance 暂无落库位置。

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/NouSart/sump/memory/redact"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// registerCallsV2 注册调用账本 / 常驻目录 / 图片索引的原生 method。
// Name 必须写成字面量：provides 门禁按字面量比对“名片承诺的动作”与“真的注册了”，
// 用循环变量注册会被它当成没注册（并抓出 provides 里多出来的承诺）。
func (s *Server) registerCallsV2() {
	callID := map[string]any{"type": "object", "properties": map[string]any{"call_id": stringSchema()}}
	controlOut := map[string]any{"type": "object", "properties": map[string]any{
		"id":         stringSchema(),
		"control_id": stringSchema(),
		"observe":    stringSchema(),
		"exec":       stringSchema(),
		"reason":     stringSchema(),
	}}
	statusOut := map[string]any{"type": "object", "properties": map[string]any{
		"owner_recovery_complete": booleanSchema(),
		"items":                   map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
	}}
	residentOut := map[string]any{"type": "object", "properties": map[string]any{
		"items": map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
		"next":  stringSchema(),
	}}
	imageOut := map[string]any{"type": "object", "properties": map[string]any{
		"image_id": stringSchema(),
		"gist":     stringSchema(),
		"status":   stringSchema(),
	}}

	s.HandleNative(spv2.Method{
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
		Output: callID,
	}, s.nativeCallOpen)

	s.HandleNative(spv2.Method{
		Name:        "call_control",
		Description: "控制一次调用（cancel 可本地执行；pause/resume 需 owner 确认，未接入 owner 客户端时回 unavailable）",
		Input: objectSchema(map[string]any{
			"id":         stringSchema(),
			"control_id": stringSchema(),
			"op":         stringSchema(),
			"ttl_ms":     integerSchema(),
		}, "id", "control_id", "op"),
		Output: controlOut,
	}, s.nativeCallControl)

	s.HandleNative(spv2.Method{
		Name:        "call_status",
		Description: "按 ids 读调用状态（对账；未接入 owner 客户端时 owner_recovery_complete 恒 false，不判 orphaned）",
		Input: objectSchema(map[string]any{
			"ids": stringArraySchema(),
		}, "ids"),
		Output: statusOut,
	}, s.nativeCallStatus)

	s.HandleNative(spv2.Method{
		Name:        "resident_list",
		Description: "列当前 owner 的常驻调用（按 work 解析 owner；owner 不同不可见）",
		Input: objectSchema(map[string]any{
			"work":   stringSchema(),
			"limit":  integerSchema(),
			"cursor": stringSchema(),
		}),
		Output: residentOut,
	}, s.nativeResidentList)

	// image_note 是唯一对外工具（S-001 §3.3），必须是 provides 里声明过的那一个。
	s.HandleNative(spv2.Method{
		Name:        "image_note",
		Tool:        true,
		Description: "记一张图的要点（gist ≤80 字、写入前脱敏；图片索引按 owner + 会话隔离）",
		Input: objectSchema(map[string]any{
			"work":     stringSchema(),
			"image_id": stringSchema(),
			"gist":     stringSchema(),
			"status":   stringSchema(),
		}, "work", "image_id"),
		Output: imageOut,
	}, s.nativeImageNote)
}

// ---------- 请求 / 响应（线路键名即契约） ----------
//
// resident_list 的 owner 按调用方 work 解析（S-001 §9）；protocol.ResidentListPayload
// 里没有 work 字段，而本次改动不允许改 protocol，所以这里用本地结构接收可选的 work：
// 缺省时回退到帧头 frame.work。

type residentListRequest struct {
	Work   string `msgpack:"work,omitempty" json:"work,omitempty"`
	Limit  int    `msgpack:"limit,omitempty" json:"limit,omitempty"`
	Cursor string `msgpack:"cursor,omitempty" json:"cursor,omitempty"`
}

type imageNoteRequest struct {
	Work    string `msgpack:"work,omitempty" json:"work,omitempty"`
	ImageID string `msgpack:"image_id,omitempty" json:"image_id,omitempty"`
	Gist    string `msgpack:"gist,omitempty" json:"gist,omitempty"`
	Status  string `msgpack:"status,omitempty" json:"status,omitempty"`
}

type imageNoteResult struct {
	ImageID string `msgpack:"image_id" json:"image_id"`
	Gist    string `msgpack:"gist" json:"gist"`
	Status  string `msgpack:"status" json:"status"`
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
	case errors.Is(err, store.ErrCallNotFound):
		return callsError(spv2.CodeNotFound, err.Error())
	case errors.Is(err, store.ErrWorkNotFound):
		return callsError(spv2.CodeNotFound, err.Error())
	case errors.Is(err, store.ErrTaskNotFound):
		return callsError(spv2.CodeNotFound, err.Error())
	case errors.Is(err, store.ErrImageNotFound):
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

// openCall 校验 work / task 后落一条本地意图（S-001 §3.0.2）。
func (s *Server) openCall(ctx context.Context, req protocol.CallOpenPayload, callerWork string, from spv2.Ref) (protocol.CallOpenResult, error) {
	var zero protocol.CallOpenResult
	work := strings.TrimSpace(req.Work)
	service := strings.TrimSpace(req.Service)
	if work == "" {
		return zero, callsError(spv2.CodeInvalidArgument, "call_open: work 必填")
	}
	if service == "" {
		return zero, callsError(spv2.CodeInvalidArgument, "call_open: service 必填")
	}
	if req.TTLMS < 0 || req.ExecDeadlineMS < 0 {
		return zero, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_open: ttl_ms / exec_deadline_ms 不能为负（收到 %d / %d）", req.TTLMS, req.ExecDeadlineMS))
	}
	mode := strings.TrimSpace(req.Mode)
	if mode == "" {
		mode = store.CallModeOnce
	}
	if mode != store.CallModeOnce && mode != store.CallModeResident {
		return zero, callsError(spv2.CodeInvalidArgument, fmt.Sprintf("call_open: mode %q 非法（once|resident）", mode))
	}

	// 校验调用方与目标 work 的归属；owner / task 从 works 行解析（calls 表不存这两列）。
	if _, err := s.authorizeTargetWork(ctx, work, callerWork, "call_open", from); err != nil {
		return zero, err
	}
	owner, task, err := s.cfg.Store.WorkOwnerTask(ctx, work)
	if err != nil {
		return zero, mapCallError(err)
	}
	owner, task = strings.TrimSpace(owner), strings.TrimSpace(task)
	if task != "" {
		if _, err := s.cfg.Store.TaskByID(task); err != nil {
			return zero, callsError(spv2.CodeNotFound,
				fmt.Sprintf("call_open: work %s 关联的 task %s 不存在", work, task))
		}
	}
	callID, err := s.cfg.Store.CreateCall(ctx, work, task, owner, "", service,
		strings.TrimSpace(req.Action), mode, req.Pausable, req.TTLMS, req.ExecDeadlineMS)
	if err != nil {
		return zero, mapCallError(err)
	}
	if s.logger != nil {
		s.logger.Printf("call_open: %s (%s/%s, mode=%s)", callID, service, req.Action, mode)
	}
	return protocol.CallOpenResult{CallID: callID}, nil
}

// controlCall 执行 call_control（S-001 §3.4 / §9）。
//
// 幂等：同 control_id 重放直接返回首次结果（call_controls 表）。
// 归属：先按调用方 work + from 做授权（authorizeCallOp），跨 work 一律 permission_denied；
// 授权在读取/回放任何控制结果之前，避免通过 control_id 探知别的 work 的调用细节。
// 控制语义按是否注入 OwnerClient 分两种：
//   - 未注入（P1 默认）：cancel 本地执行；pause / resume 回 unavailable，不改状态；
//   - 已注入：pause / resume / cancel 的“成功”都以 owner 确认为准；owner 报错 / 超时
//     （含 ACK 丢失）一律按未确认处理，不写 paused / running 之类的假状态。
func (s *Server) controlCall(ctx context.Context, req protocol.CallControlPayload, callerWork string, from spv2.Ref) (protocol.CallControlResult, error) {
	var zero protocol.CallControlResult
	id := strings.TrimSpace(req.ID)
	controlID := strings.TrimSpace(req.ControlID)
	op := strings.ToLower(strings.TrimSpace(req.Op))
	if id == "" || controlID == "" || op == "" {
		return zero, callsError(spv2.CodeInvalidArgument, "call_control: id / control_id / op 必填")
	}
	switch op {
	case "pause", "resume", "cancel":
	default:
		return zero, callsError(spv2.CodeInvalidArgument, fmt.Sprintf("call_control: op %q 非法（pause|resume|cancel）", op))
	}

	// 先读目标 call 并做归属校验：跨 work 的 id（含重放路径）不接受。
	callRow, err := s.cfg.Store.GetCall(ctx, id)
	if err != nil {
		return zero, mapCallError(err)
	}
	if err := s.authorizeCallOp(ctx, callRow, callerWork, from, "call_control"); err != nil {
		return zero, err
	}

	// 幂等重放：control_id 已存在时只看首次结果，不再执行。
	if existing, ok, err := s.cfg.Store.GetControl(ctx, controlID); err != nil {
		return zero, mapCallError(err)
	} else if ok {
		if existing.CallID != id || existing.Op != op {
			// 不回显别的调用的 id / op：control_id 是调用方给的，不能借冲突错误泄漏他人参数。
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: control_id %s 已被其它调用或操作使用", controlID))
		}
		if existing.Result == "" {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: control_id %s 的首次结果尚未落库，不能重放", controlID))
		}
		first, err := store.DecodeControlResult(existing.Result)
		if err != nil {
			return zero, mapCallError(err)
		}
		return protocol.CallControlResult{ID: id, ControlID: controlID,
			Observe: first.Observe, Exec: first.Exec, Reason: first.Reason}, nil
	}

	if callRow.Observe == store.CallObserveClosed {
		return zero, callsError(spv2.CodeConflict, fmt.Sprintf("call_control: call %s 已关闭", id))
	}

	switch op {
	case "pause":
		if !callRow.Pausable {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: call %s 不可暂停（pausable=false）", id))
		}
		return s.pauseCall(ctx, controlID, callRow, req.TTLMS)
	case "resume":
		if callRow.Exec != store.CallExecPaused {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: resume 只能从 paused → running（当前 exec=%s）", callRow.Exec))
		}
		return s.resumeCall(ctx, controlID, callRow, req.TTLMS)
	case "cancel":
		if isTerminalExec(callRow.Exec) {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: call %s 已结束（exec=%s），不能再取消", id, callRow.Exec))
		}
		return s.cancelCall(ctx, controlID, callRow, req.TTLMS)
	}
	return zero, callsError(spv2.CodeInternal, "call_control: 未处理的分支 "+op)
}

// authorizeCallOp 判定调用方可否控制 / 查询目标 call（S-001 §9；R-011 §2.2 红队修复）。
//
// 红队问题：同一个 agent 服务同时服务多个 work 时，仅按 works.agent.id 比对会允许
// w-alice 的 agent 拿 w-bob 的 call_id 取消别人的调用。因此这里要求：
//   - 普通调用方（agent / 入口 / 服务）：调用方 work 必须等于目标 call 的 work，
//     且身份属于该 work（agent / owner / entry，与 authorizeWork 同口径）；
//   - 人类 owner 入口（from.Authority=owner 且 works.owner 与 from.ID 匹配）：可跨 work
//     处置自己的调用（用户从自己的入口取消时，调用方 work 可能不是目标 work）；
//   - 运行期管控（runtime authority）：保留运维处置通道，可跨 work；
//   - 其它一律 permission_denied（不用 not_found，便于定位；错误文案不泄漏目标参数）。
func (s *Server) authorizeCallOp(ctx context.Context, row store.CallRow, callerWork string, from spv2.Ref, op string) error {
	w, err := s.cfg.Store.GetWork(ctx, row.Work)
	if err != nil {
		if errors.Is(err, store.ErrWorkNotFound) {
			// work 行缺失：没有可核对的归属，fail-closed。
			return callsError(spv2.CodePermissionDenied, op+": 目标调用的 work 归属不可核对，拒绝操作")
		}
		return mapCallError(err)
	}
	if from.Authority == spv2.AuthorityRuntime {
		return nil
	}
	fromID := strings.TrimSpace(from.ID)
	if from.Authority == spv2.AuthorityOwner {
		// owner 权限必须精确对应 works.owner，不能凭 authority 字段跨 work。
		if fromID != "" && fromID == strings.TrimSpace(w.Owner.ID) {
			return nil
		}
		return callsError(spv2.CodePermissionDenied, op+": 无权操作该调用（owner 与调用方不匹配）")
	}
	if strings.TrimSpace(callerWork) != strings.TrimSpace(w.ID) {
		return callsError(spv2.CodePermissionDenied, op+": 调用方 work 与目标调用不一致，拒绝跨 work 操作")
	}
	if err := authorizeWork(w, from); err != nil {
		return callsError(spv2.CodePermissionDenied, op+": 无权操作该 work 的调用（调用方不是该 work 的 agent / 入口）")
	}
	return nil
}

// pauseCall 向 owner 发起暂停握手（S-001 §3.4 / §5.1）。
//
// 只有 owner 明确回答 paused（或空串确认）才写 paused。报错 / 超时 / 非预期回答都只把
// 观察维置为 not_waiting 并记 pause_failed：执行维与 phase 保持原值——ACK 丢失时
// owner 可能其实已经暂停，但 memory 不能据此写 paused，更不能写终态 phase。
func (s *Server) pauseCall(ctx context.Context, controlID string, row store.CallRow, ttlMS int64) (protocol.CallControlResult, error) {
	client := s.ownerClient
	if client == nil {
		// 未注入 owner：没有谁能确认暂停，不能本地假装成功（否则会误导门禁与 owner 恢复）。
		return protocol.CallControlResult{}, callsError(spv2.CodeUnavailable,
			"call_control: pause 需要 owner 确认；当前未注入执行服务客户端，不能标记 paused")
	}
	// pause 只对“已受理”的调用有意义：created 是本地意图，还没派发（§3.0.2）。
	if row.Exec != store.CallExecRunning && row.Exec != store.CallExecUnknown {
		return protocol.CallControlResult{}, callsError(spv2.CodeConflict,
			fmt.Sprintf("call_control: pause 只能作用于已受理的调用（当前 exec=%s）", row.Exec))
	}
	cctx, cancel := ownerControlContext(ctx, ttlMS)
	defer cancel()
	confirmedExec, err := client.Pause(cctx, row.Service, row.ID)
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
	client := s.ownerClient
	if client == nil {
		return protocol.CallControlResult{}, callsError(spv2.CodeUnavailable,
			"call_control: resume 需要 owner 确认；当前未注入执行服务客户端，不能标记 running")
	}
	cctx, cancel := ownerControlContext(ctx, ttlMS)
	defer cancel()
	confirmedExec, err := client.Resume(cctx, row.Service, row.ID)
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
// 观察维一律 closed（本地不再等）；执行维分三种：
//   - 未注入 owner（P1 本地语义）：可暂停 → cancelled；不可暂停 → unknown + remote_may_continue；
//   - 注入 owner 且不可暂停：不发控制（发了也停不下来），closed/unknown + remote_may_continue，
//     reason=cancelled_local；
//   - 注入 owner 且可暂停：问 owner；确认 → closed/cancelled；未确认（报错/超时/ACK 丢失）→
//     closed/unknown + remote_may_continue，reason=cancel_unconfirmed，不写 phase。
func (s *Server) cancelCall(ctx context.Context, controlID string, row store.CallRow, ttlMS int64) (protocol.CallControlResult, error) {
	var exec, phase, reason string
	remoteMayContinue := false

	switch {
	case s.ownerClient == nil:
		// P1 诚实边界：保留本地取消语义（没有 owner 可问）。
		if row.Pausable {
			exec, phase, reason = store.CallExecCancelled, store.CallPhaseCancelled, "cancelled"
		} else {
			exec, phase, reason = store.CallExecUnknown, "", "remote_may_continue"
			remoteMayContinue = true
		}
	case !row.Pausable:
		// 不可暂停调用：取消只能保证 memory 不再等，远端可能还在继续（S-001 §3.4）。
		exec, phase, reason = store.CallExecUnknown, "", "cancelled_local"
		remoteMayContinue = true
	default:
		cctx, cancel := ownerControlContext(ctx, ttlMS)
		defer cancel()
		confirmedExec, err := s.ownerClient.Cancel(cctx, row.Service, row.ID)
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
// 注入后按 owner service 分批询问：
//   - owner 报错 / 超时（离线）：整体 recovery_complete=false，本地观测原样返回，
//     **不**把“问不到”判 orphaned（§3.5 规则 1）；
//   - owner 恢复完整（recoveryComplete=true）但明确不认识某个本地已知 call：
//     执行真相丢失（§5.4 灾难路径），报告 exec=unknown、phase=orphaned；
//   - 本地账本完全查不到的 id：没有 service 可问，保持 unknown（不回 exec/phase，
//     更不判 orphaned）——这是“问不到 ≠ 不存在”的硬边界；
//   - owner 认识的 call 按其返回的 exec 上报（只读对账；落地策略另见文件头）。
//   - 归属：与 call_control 同口径（authorizeCallOp）逐 id 校验；任一本地已知 id
//     跨 work / 身份不符即整体 permission_denied，不通过 items 泄漏别的 work 的调用。
func (s *Server) callStatus(ctx context.Context, req protocol.CallStatusPayload, callerWork string, from spv2.Ref) (protocol.CallStatusResult, error) {
	out := protocol.CallStatusResult{OwnerRecoveryComplete: false, Items: []protocol.CallStatusItem{}}
	if len(req.IDs) == 0 {
		return out, callsError(spv2.CodeInvalidArgument, "call_status: ids 不能为空")
	}
	if len(req.IDs) > 100 {
		return out, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_status: 单批最多 100 个 id（收到 %d）", len(req.IDs)))
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
		// 归属校验：跨 work 的已知 id 直接拒绝（S-001 §9；R-011 §2.2）。
		if err := s.authorizeCallOp(ctx, row, callerWork, from, "call_status"); err != nil {
			return out, err
		}
		entries = append(entries, statusEntry{id: id, row: row, ok: true})
		if row.Service != "" {
			byService[row.Service] = append(byService[row.Service], row.ID)
		}
	}

	client := s.ownerClient
	if client == nil || len(byService) == 0 {
		// 未接 owner，或没有任何“本地已知 call”可提供 owner service：
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

	// 按 owner service 分批问；任一 service 未确认就不把“不认识”当 orphaned（保守取 AND）。
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
		if exec, known := states[e.row.ID]; known && strings.TrimSpace(exec) != "" {
			item.Exec = exec
		} else if recoveryComplete {
			// owner 恢复完整且明确不认识这个本地已知 call：执行真相丢失（§5.4 灾难路径）。
			// 只在报告层标 orphaned；本地账本不动（落地策略未定，见文件头）。
			item.Exec = store.CallExecUnknown
			item.Phase = store.CallPhaseOrphaned
		}
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

// residentList 列当前 owner 的常驻调用（S-001 §3.6 / §9）。
// owner 来自调用方 work（payload.work，缺省 frame.work）；解析不到或越权回 permission_denied。
func (s *Server) residentList(ctx context.Context, req residentListRequest, from spv2.Ref) (protocol.ResidentListResult, error) {
	out := protocol.ResidentListResult{Items: []protocol.ResidentItem{}}
	work := strings.TrimSpace(req.Work)
	if work == "" {
		return out, callsError(spv2.CodePermissionDenied,
			"resident_list: 无法解析调用方 work（payload.work 与 frame.work 都为空）")
	}
	w, err := s.cfg.Store.GetWork(ctx, work)
	if err != nil {
		return out, mapCallError(err)
	}
	if err := authorizeWork(w, from); err != nil {
		return out, err
	}
	owner := strings.TrimSpace(w.Owner.ID)
	if owner == "" {
		return out, callsError(spv2.CodePermissionDenied,
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
		// origin_session 用不透明会话 ID（跨会话展示不泄露原值，S-001 §9）。
		origin := ""
		if row.Conversation != "" {
			if opaque, err := redact.SessionID(row.Conversation); err == nil {
				origin = opaque
			}
		}
		title := row.Action
		if title == "" {
			title = row.Service
		}
		out.Items = append(out.Items, protocol.ResidentItem{
			ID:            row.ID,
			Kind:          "call",
			Title:         title,
			Observe:       row.Observe,
			Exec:          row.Exec,
			Phase:         row.Phase,
			OwnerService:  row.Service,
			OriginSession: origin,
			CreatedAt:     row.CreatedAt,
			UpdatedAt:     row.UpdatedAt,
		})
	}
	if limit > 0 && len(rows) == limit {
		out.Next = rows[len(rows)-1].ID
	}
	return out, nil
}

// imageNote 写入一条图片要点（S-001 §3.3）：gist 先脱敏、按字符截到 80。
// owner 从目标 work 解析，conversation 从 work 关联的 task 解析；
// 调用方 work 的 owner 必须与目标 work 一致（跨 owner 拒绝）。
func (s *Server) imageNote(ctx context.Context, req imageNoteRequest, from spv2.Ref, callerWork string) (imageNoteResult, error) {
	var zero imageNoteResult
	work := strings.TrimSpace(req.Work)
	imageID := strings.TrimSpace(req.ImageID)
	if work == "" || imageID == "" {
		return zero, callsError(spv2.CodeInvalidArgument, "image_note: work / image_id 必填")
	}
	status := strings.TrimSpace(req.Status)
	if status == "" {
		status = store.ImageStatusStored
	}
	if status != store.ImageStatusStored && status != store.ImageStatusLost {
		return zero, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("image_note: status %q 非法（stored|lost）", status))
	}

	w, err := s.cfg.Store.GetWork(ctx, work)
	if err != nil {
		return zero, mapCallError(err)
	}
	if err := authorizeWork(w, from); err != nil {
		return zero, err
	}
	owner, task, err := s.cfg.Store.WorkOwnerTask(ctx, work)
	if err != nil {
		return zero, mapCallError(err)
	}
	owner = strings.TrimSpace(owner)
	if owner == "" {
		return zero, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("image_note: work %s 没有 owner，无法写入图片索引", work))
	}

	// 越权检查：调用方 work 与目标 work 必须同 owner（图片索引按 owner 隔离）。
	callerWork = strings.TrimSpace(callerWork)
	if callerWork == "" {
		return zero, callsError(spv2.CodePermissionDenied, "image_note: 无法解析调用方 work，拒绝写入")
	}
	callerOwner, _, err := s.cfg.Store.WorkOwnerTask(ctx, callerWork)
	if err != nil {
		if errors.Is(err, store.ErrWorkNotFound) {
			return zero, callsError(spv2.CodePermissionDenied,
				fmt.Sprintf("image_note: 调用方 work %s 不存在，拒绝写入", callerWork))
		}
		return zero, mapCallError(err)
	}
	if strings.TrimSpace(callerOwner) == "" || strings.TrimSpace(callerOwner) != owner {
		return zero, callsError(spv2.CodePermissionDenied,
			fmt.Sprintf("image_note: 调用方 owner %q 与 work owner %q 不一致，拒绝跨 owner 写入", callerOwner, owner))
	}

	if task == "" {
		return zero, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("image_note: work %s 未关联 task，无法解析对应会话", work))
	}
	taskRow, err := s.cfg.Store.TaskByID(task)
	if err != nil || strings.TrimSpace(taskRow.ConversationID) == "" {
		return zero, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("image_note: work %s 的 task %s 解析不到会话", work, task))
	}

	// gist：先过 redact.Text，再按字符截到 80（redact.Sanitize = Text + Truncate）。
	gist := redact.Sanitize(req.Gist, 80)
	if err := s.cfg.Store.UpsertImageIndex(ctx, owner, taskRow.ConversationID, imageID, gist, status); err != nil {
		return zero, mapCallError(err)
	}
	return imageNoteResult{ImageID: imageID, Gist: gist, Status: status}, nil
}

// ---------- 原生 handler（解码 / 回包） ----------

func (s *Server) nativeCallOpen(ctx context.Context, call *spv2.Call) error {
	var req protocol.CallOpenPayload
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "call_open body 无效: "+err.Error(), false, nil)
	}
	out, err := s.openCall(ctx, req, call.WorkID(), call.Frame.From)
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeCallControl(ctx context.Context, call *spv2.Call) error {
	var req protocol.CallControlPayload
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "call_control body 无效: "+err.Error(), false, nil)
	}
	out, err := s.controlCall(ctx, req, call.WorkID(), call.Frame.From)
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeCallStatus(ctx context.Context, call *spv2.Call) error {
	var req protocol.CallStatusPayload
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "call_status body 无效: "+err.Error(), false, nil)
	}
	out, err := s.callStatus(ctx, req, call.WorkID(), call.Frame.From)
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
	out, err := s.residentList(ctx, req, call.Frame.From)
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeImageNote(ctx context.Context, call *spv2.Call) error {
	var req imageNoteRequest
	if err := call.Frame.DecodeBody(&req); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "image_note body 无效: "+err.Error(), false, nil)
	}
	out, err := s.imageNote(ctx, req, call.Frame.From, call.WorkID())
	if err != nil {
		return replyCallsError(call, err)
	}
	return call.Reply(out)
}
