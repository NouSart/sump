package server

// 调用账本 / 常驻目录 / 图片索引的原生入口（S-001 §3.3 / §3.4 / §3.5 / §3.6 / §9）。
//
// 注册入口单独抽成 registerCallsV2()：server.New 只负责调用它（接线由主代理完成），
// 与既有动作互不纠缠；测试也能单独校验 method 清单与逻辑层。
//
// P1 的诚实边界（与 store/calls.go 文件头一致）：
//   - pause / resume 需要 owner 确认，当前没有接入 owner 客户端；
//     这里明确返回 unavailable，**不**把状态改成 paused / running。
//   - call_status 的 owner_recovery_complete 恒为 false：owner 对账未接入，
//     不能把“不认识这个 call”判成 orphaned（S-001 §3.5 规则 1）。
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

// callsV2Methods 是 calls_v2.go 注册的全部原生 method（登记与服务卡 schema 的唯一来源）。
// Tool 标记按 S-001：只有 image_note 是工具（§3.3），其余是内部 / 调试 method。
func callsV2Methods() []spv2.Method {
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
	return []spv2.Method{
		{
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
		},
		{
			Name:        "call_control",
			Description: "控制一次调用（cancel 可本地执行；pause/resume 需 owner 握手，当前回 unavailable）",
			Input: objectSchema(map[string]any{
				"id":         stringSchema(),
				"control_id": stringSchema(),
				"op":         stringSchema(),
				"ttl_ms":     integerSchema(),
			}, "id", "control_id", "op"),
			Output: controlOut,
		},
		{
			Name:        "call_status",
			Description: "按 ids 读调用状态（对账；owner_recovery_complete 恒 false，不判 orphaned）",
			Input: objectSchema(map[string]any{
				"ids": stringArraySchema(),
			}, "ids"),
			Output: statusOut,
		},
		{
			Name:        "resident_list",
			Description: "列当前 owner 的常驻调用（按 work 解析 owner；owner 不同不可见）",
			Input: objectSchema(map[string]any{
				"work":   stringSchema(),
				"limit":  integerSchema(),
				"cursor": stringSchema(),
			}),
			Output: residentOut,
		},
		{
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
		},
	}
}

// registerCallsV2 注册调用账本 / 常驻目录 / 图片索引的原生 method。
// 调用点由 server.New 负责（本文件不修改 server.go）。
func (s *Server) registerCallsV2() {
	for _, m := range callsV2Methods() {
		switch m.Name {
		case "call_open":
			s.HandleNative(m, s.nativeCallOpen)
		case "call_control":
			s.HandleNative(m, s.nativeCallControl)
		case "call_status":
			s.HandleNative(m, s.nativeCallStatus)
		case "resident_list":
			s.HandleNative(m, s.nativeResidentList)
		case "image_note":
			s.HandleNative(m, s.nativeImageNote)
		default:
			if s.logger != nil {
				s.logger.Printf("忽略未知的 calls_v2 method: %s", m.Name)
			}
		}
	}
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
func (s *Server) openCall(ctx context.Context, req protocol.CallOpenPayload) (protocol.CallOpenResult, error) {
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

	// 校验 work 存在，并取 works 里的 owner / task（calls 表不存这两列）。
	owner, task, err := s.cfg.Store.WorkOwnerTask(ctx, work)
	if err != nil {
		return zero, mapCallError(err)
	}
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

// controlCall 执行 call_control（S-001 §3.4）。
//
// 幂等：同 control_id 重放直接返回首次结果（call_controls 表）。
// cancel 可以本地执行；pause / resume 必须 owner 确认，P1 返回 unavailable 且不改状态。
func (s *Server) controlCall(ctx context.Context, req protocol.CallControlPayload) (protocol.CallControlResult, error) {
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

	// 幂等重放：control_id 已存在时只看首次结果，不再执行。
	if existing, ok, err := s.cfg.Store.GetControl(ctx, controlID); err != nil {
		return zero, mapCallError(err)
	} else if ok {
		if existing.CallID != id || existing.Op != op {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: control_id %s 已用于 call %s op %s", controlID, existing.CallID, existing.Op))
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

	callRow, err := s.cfg.Store.GetCall(ctx, id)
	if err != nil {
		return zero, mapCallError(err)
	}
	if callRow.Observe == store.CallObserveClosed {
		return zero, callsError(spv2.CodeConflict, fmt.Sprintf("call_control: call %s 已关闭", id))
	}

	switch op {
	case "cancel":
		if isTerminalExec(callRow.Exec) {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: call %s 已结束（exec=%s），不能再取消", id, callRow.Exec))
		}
		observe := store.CallObserveClosed
		var exec, phase, reason string
		remoteMayContinue := false
		if callRow.Pausable {
			// 可暂停调用：本地取消即视为执行侧已收到取消（owner 受理后取消是幂等语义）。
			exec, phase, reason = store.CallExecCancelled, store.CallPhaseCancelled, "cancelled"
		} else {
			// 不可暂停调用：取消只能保证“不再等”，远端可能还在继续（S-001 §3.4）。
			// 不写 phase，并显式标 remote_may_continue。
			exec, phase, reason = store.CallExecUnknown, "", "remote_may_continue"
			remoteMayContinue = true
		}
		if err := s.cfg.Store.SetCallState(ctx, id, observe, exec, phase, reason); err != nil {
			return zero, mapCallError(err)
		}
		if remoteMayContinue {
			if err := s.cfg.Store.SetCallRemoteMayContinue(ctx, id, true); err != nil {
				return zero, mapCallError(err)
			}
		}
		res := protocol.CallControlResult{ID: id, ControlID: controlID,
			Observe: observe, Exec: exec, Reason: reason}
		if err := s.recordControl(ctx, controlID, id, op, res); err != nil {
			return zero, mapCallError(err)
		}
		return res, nil

	case "pause":
		if !callRow.Pausable {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: call %s 不可暂停（pausable=false）", id))
		}
		// TODO(S-001 §3.4 暂停握手)：等 owner 客户端接入后，这里发 pause 控制帧并等 ACK；
		// ACK 丢失按 exec=unknown、observe=not_waiting、不写 phase 处理；只有确认后才写 paused。
		// 当前没有执行服务可确认，不能本地假装成功（否则会误导门禁与 owner 恢复）。
		return zero, callsError(spv2.CodeUnavailable,
			"call_control: pause 需要 owner 确认；当前未接入执行服务的暂停握手，不能标记 paused")

	case "resume":
		if callRow.Exec != store.CallExecPaused {
			return zero, callsError(spv2.CodeConflict,
				fmt.Sprintf("call_control: resume 只能从 paused → running（当前 exec=%s）", callRow.Exec))
		}
		// TODO(S-001 §3.4 暂停握手)：与 pause 对称，等 owner 确认后才写 running。
		return zero, callsError(spv2.CodeUnavailable,
			"call_control: resume 需要 owner 确认；当前未接入执行服务的恢复握手，不能标记 running")
	}
	return zero, callsError(spv2.CodeInternal, "call_control: 未处理的分支 "+op)
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

// callStatus 按 ids 读调用状态（对账读取；S-001 §3.5）。
//
// owner_recovery_complete 恒 false：owner 对账尚未接入。调用方不得据此把
// “不认识该 call” 判成 orphaned——items 里查不到的 id 只回 id 本身。
func (s *Server) callStatus(ctx context.Context, req protocol.CallStatusPayload) (protocol.CallStatusResult, error) {
	out := protocol.CallStatusResult{OwnerRecoveryComplete: false, Items: []protocol.CallStatusItem{}}
	if len(req.IDs) == 0 {
		return out, callsError(spv2.CodeInvalidArgument, "call_status: ids 不能为空")
	}
	if len(req.IDs) > 100 {
		return out, callsError(spv2.CodeInvalidArgument,
			fmt.Sprintf("call_status: 单批最多 100 个 id（收到 %d）", len(req.IDs)))
	}
	for _, raw := range req.IDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		row, err := s.cfg.Store.GetCall(ctx, id)
		if err != nil {
			if errors.Is(err, store.ErrCallNotFound) {
				// 查不到不等于 orphaned：owner 恢复未完成（S-001 §3.5 规则 1）。
				out.Items = append(out.Items, protocol.CallStatusItem{ID: id})
				continue
			}
			return out, mapCallError(err)
		}
		out.Items = append(out.Items, protocol.CallStatusItem{
			ID:           row.ID,
			Observe:      row.Observe,
			Exec:         row.Exec,
			Phase:        row.Phase,
			OwnerService: row.Service,
			UpdatedAt:    row.UpdatedAt,
		})
	}
	return out, nil
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
	if strings.TrimSpace(callerOwner) != "" && strings.TrimSpace(callerOwner) != owner {
		return zero, callsError(spv2.CodePermissionDenied,
			fmt.Sprintf("image_note: 调用方 owner %q 与 work owner %q 不同，拒绝跨 owner 写入", callerOwner, owner))
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
	out, err := s.openCall(ctx, req)
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
	out, err := s.controlCall(ctx, req)
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
	out, err := s.callStatus(ctx, req)
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
