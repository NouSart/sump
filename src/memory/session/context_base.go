package session

// context_base 与 trigger_complete（S-001 §3.1 / §3.1.1）。
//
// memory 是唯一组装者（INV-1 / INV-3）：调用方只给 work + 触发源，拿回一份带
// snapshot_id 的消息链。同一触发只能组装一次——`trigger_states` 决定重复触发是
// 「沿用快照」「只补 ACK」还是「拒绝重建」，`context_snapshots` 保存那份不可变投影。
//
// R-026-D1=(a)：快照只存**基座**（messages / task / work），不冻 experience；
// experience 由 facade 在 assembled 快照之上每次现注入。completed 只补 ACK，不读快照、
// 不注入 experience（R-026-D2）。
//
// work / task 登记不在这里：reasoner 建链前先调 work_open（session.work_open），
// 这里只读已存在的 work 与它关联的任务；work 没接任务就显式 not_found，不静默新建。
//
// 授权（R-023 P4）：两个入口都先过 authorizeTargetWork——载荷 work 必填，帧头 work 必须
// 与它一致，身份必须属于该 work（owner / runtime 另有跨 work 通道）；匿名一律拒绝。
// 授权发生在读触发 / 落快照之前，越权请求不产生快照、不改触发状态。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/NouSart/sump/memory/session/assemble"
	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

const (
	// contextBaseSchema 是本方法的载荷版本（S-001 §3.1 的 "context/2"）。
	contextBaseSchema = "context/2"
	// contextBaseSnapshotTTL 是快照保留期默认值（S-001 §13：24h，先不做配置化）。
	contextBaseSnapshotTTL = 24 * time.Hour
)

// ---------- 注册 ----------

// registerContextBase 注册 context_base 与 trigger_complete（在 registerNative 里调用）。
// 两个都不是工具（Tool=false）：模型不该自己决定「要不要重开一轮」或「这轮算不算完」。
func (s *Server) registerContextBase() {
	s.HandleNative(contextBaseMethod(), s.nativeContextBase)
	s.HandleNative(triggerCompleteMethod(), s.nativeTriggerComplete)
}

// contextBaseMethod 是 context_base 的服务卡声明（独立成函数，测试可直接校验 schema）。
func contextBaseMethod() spv2.Method {
	return spv2.Method{
		Name:        "context_base",
		Description: "按 work + 触发源取组装好的上下文；同一触发复用同一快照（不重复组装）",
		Input: objectSchema(map[string]any{
			"work": stringSchema(),
			"trigger": objectSchema(map[string]any{
				"kind": stringSchema(),
				"item": stringSchema(),
			}, "kind", "item"),
			"schema": stringSchema(),
		}, "work", "trigger"),
		Output: objectSchema(map[string]any{
			"schema":        stringSchema(),
			"snapshot_id":   stringSchema(),
			"expires_at":    stringSchema(),
			"messages":      map[string]any{"type": "array", "items": map[string]any{"type": "object"}},
			"task":          map[string]any{"type": "object"},
			"work":          map[string]any{"type": "object"},
			"degraded":      map[string]any{"type": "object"},
			"warnings":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			"assembled_at":  stringSchema(),
			"trigger_state": stringSchema(),
			"completed":     booleanSchema(),
		}, "schema", "trigger_state", "snapshot_id"),
		Tool: false,
	}
}

// triggerCompleteMethod 是 trigger_complete 的服务卡声明。
func triggerCompleteMethod() spv2.Method {
	return spv2.Method{
		Name:        "trigger_complete",
		Description: "标记本轮触发已完成（幂等；必须在模型输出与后续动作安全落库后调用）",
		Input: objectSchema(map[string]any{
			"work": stringSchema(),
			"trigger": objectSchema(map[string]any{
				"kind": stringSchema(),
				"item": stringSchema(),
			}, "kind", "item"),
		}, "work", "trigger"),
		Output: objectSchema(map[string]any{
			"completed": booleanSchema(),
		}, "completed"),
		Tool: false,
	}
}

// ---------- 原生入口 ----------

func (s *Server) nativeContextBase(ctx context.Context, call *spv2.Call) error {
	out, err := s.contextBase(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyContextBaseError(call, err)
	}
	return call.Reply(out)
}

func (s *Server) nativeTriggerComplete(ctx context.Context, call *spv2.Call) error {
	out, err := s.triggerComplete(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyContextBaseError(call, err)
	}
	return call.Reply(out)
}

// replyContextBaseError 把共享实现里的错误翻成线上错误码：
// 业务语义错误（invalid_argument / not_found / conflict）原样保留，其余按 internal。
func replyContextBaseError(call *spv2.Call, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return call.ReplyError(e.Code, e.Message, e.Retryable, e.Details)
	}
	return call.ReplyError(spv2.CodeInternal, err.Error(), true, nil)
}

// v2Error 构造一个不可重试的业务错误。
func v2Error(code, format string, args ...any) error {
	return spv2.ErrCode(code, fmt.Sprintf(format, args...), false, nil)
}

// ---------- context_base ----------

// contextBase 是 context_base 的共享实现（handler 只做转发）。
//
// 分支（S-001 §3.1 规则 2）：
//   - pending / 没有记录：首次组装，落快照 + 置 assembled；
//   - assembled：沿用快照，不重新查库、不重新组装；
//   - completed：只补 ACK——completed=true 且**不带消息**，快照清理后依然成立，
//     重复取上下文不会把已经收口的那一轮重新推理一遍（§3.1.1）；
//   - expired：not_found，只能显式 resume 新触发，不得静默重建。
func (s *Server) contextBase(ctx context.Context, body []byte, c caller) (any, error) {
	var req protocol.ContextRequest
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return nil, v2Error(spv2.CodeInvalidArgument, "context_base body 无效: %v", err)
	}
	work := strings.TrimSpace(req.Work)
	kind := strings.TrimSpace(req.Trigger.Kind)
	item := strings.TrimSpace(req.Trigger.Item)
	if work == "" {
		return nil, v2Error(spv2.CodeInvalidArgument, "context_base: work 必填")
	}
	if kind == "" || item == "" {
		return nil, v2Error(spv2.CodeInvalidArgument, "context_base: trigger.kind 与 trigger.item 必填")
	}
	if req.Schema != "" && req.Schema != contextBaseSchema {
		return nil, v2Error(spv2.CodeInvalidArgument,
			"context_base: 不支持的 schema %q（只支持 %s）", req.Schema, contextBaseSchema)
	}
	// 授权在触发存在性 / 快照读取之前：越权请求不能借状态差异探测别人的 work。
	w, err := s.authorizeTargetWork(ctx, work, c.work, "context_base", c.from)
	if err != nil {
		return nil, err
	}
	if spv2.IsTerminal(w.Status) {
		return nil, v2Error(spv2.CodeConflict,
			"context_base: work %s 已终态（%s），不能再取上下文", work, w.Status)
	}

	// 触发条目必须已落库：没落库的 item 会让快照绑定到一个不存在的事实上。
	exists, err := s.cfg.Store.TriggerItemExists(ctx, work, item)
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	if !exists {
		return nil, v2Error(spv2.CodeInvalidArgument,
			"context_base: trigger.item %s 未落库或不属于 work %s", item, work)
	}

	state, snapshotID, err := s.cfg.Store.GetTriggerState(ctx, work, kind, item)
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	switch state {
	case store.TriggerStateCompleted:
		// 已完成：只补 ACK。这一步的意义在于「不做事」——不重新组装、不再灌一遍历史，
		// 否则 agent 会以为拿到了新一轮的上下文，把已经收口的对话再推理一次（§3.1.1）。
		return completedTriggerAck(snapshotID), nil
	case store.TriggerStateAssembled:
		// 同一轮的续跑：沿用那份不可变投影，不重新查库、不重新组装。
		return s.loadContextBaseSnapshot(ctx, work, kind, item, state)
	case "", store.TriggerStatePending:
		return s.assembleContextBase(ctx, work, kind, item, time.Now())
	case store.TriggerStateExpired:
		return nil, v2Error(spv2.CodeNotFound,
			"context_base: 触发（%s/%s）的快照已过期，只能显式 resume 新触发，不能静默重建", kind, item)
	default:
		return nil, v2Error(spv2.CodeInternal, "context_base: 未知触发状态 %q", state)
	}
}

// completedTriggerAck 是「本轮已完成」的应答：只表明状态，不带消息。
//
// 快照可能已经被清理（保留期 24h），但**已完成这件事不会因为快照没了而退化成没做**，
// 所以这里不读快照、也不因快照缺失报 not_found。
func completedTriggerAck(snapshotID string) protocol.ContextResponse {
	return protocol.ContextResponse{
		Schema:       contextBaseSchema,
		SnapshotID:   snapshotID,
		TriggerState: store.TriggerStateCompleted,
		Completed:    true,
		Warnings:     []string{"本轮触发已完成，只补 ACK；要开始新一轮请换 trigger.item"},
	}
}

// loadContextBaseSnapshot 读已有快照并按原载荷返回。
// 快照缺失 / 过期一律 not_found：这是「显式失败」，不得退化成重新组装。
func (s *Server) loadContextBaseSnapshot(ctx context.Context, work, kind, item, state string) (protocol.ContextResponse, error) {
	_, payload, _, err := s.cfg.Store.LoadSnapshot(ctx, work, kind, item)
	switch {
	case errors.Is(err, store.ErrSnapshotNotFound):
		return protocol.ContextResponse{}, v2Error(spv2.CodeNotFound,
			"context_base: 触发（%s/%s）状态为 %s，但快照不存在；不能静默重建", kind, item, state)
	case errors.Is(err, store.ErrSnapshotExpired):
		return protocol.ContextResponse{}, v2Error(spv2.CodeNotFound,
			"context_base: 触发（%s/%s）状态为 %s，但快照已过期；不能静默重建", kind, item, state)
	case err != nil:
		return protocol.ContextResponse{}, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	var resp protocol.ContextResponse
	if err := protocol.DecodeRaw(payload, &resp); err != nil {
		return protocol.ContextResponse{}, v2Error(spv2.CodeInternal, "context_base: 快照载荷无法解析: %v", err)
	}
	// 状态以 trigger_states 为准（快照是那份不可变投影，不随身边的状态改）。
	resp.TriggerState = state
	return resp, nil
}

// assembleContextBase 首次组装：读 work / task / 消息 → assemble.Assemble →
// 落快照 → 置 assembled。并发首次触发时，SaveSnapshot 的唯一约束决定谁赢；
// 输的一方沿用赢家那份快照（同一触发只能有一份投影）。
func (s *Server) assembleContextBase(ctx context.Context, work, kind, item string, now time.Time) (any, error) {
	taskID, err := s.cfg.Store.TaskIDOfWork(ctx, work)
	if errors.Is(err, store.ErrWorkNotFound) {
		return nil, v2Error(spv2.CodeNotFound, "context_base: work %s 不存在", work)
	}
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	if taskID == "" {
		return nil, v2Error(spv2.CodeNotFound,
			"context_base: work %s 未关联任务（先调 work_open 登记 work 与任务）", work)
	}
	task, err := s.cfg.Store.TaskByID(taskID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, v2Error(spv2.CodeNotFound, "context_base: work %s 所属任务 %q 不存在", work, taskID)
	}
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	w, err := s.cfg.Store.GetWork(ctx, work)
	if spv2.IsCode(err, spv2.CodeNotFound) {
		return nil, v2Error(spv2.CodeNotFound, "context_base: work %s 不存在", work)
	}
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}

	// history_limit 是「每次带上多少条旧对话」：0 = 不带历史（trigger 本身仍需已落库）。
	// RecentMessagesByWork 的 0 表示默认 50，所以这里显式区分「不限」与「不要」。
	var rows []store.MessageRow
	if limit := s.currentHistoryLimit(); limit != 0 {
		rows, err = s.cfg.Store.RecentMessagesByWork(ctx, work, limit)
		if err != nil {
			return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
		}
	}
	history, toolCalls, toolResults := recordsFromMessageRows(rows)

	out, err := assemble.Assemble(assemble.Input{
		WorkID:         work,
		TaskID:         task.ID,
		ConversationID: task.ConversationID,
		Owner:          w.Owner.ID,
		Entry:          w.Entry.ID,
		TaskName:       task.Name,
		History:        history,
		ToolCalls:      toolCalls,
		ToolResults:    toolResults,
		// Images 传空：image_index 未搬（R-023 D7），图片通道归 images 服务。
		Config:  assemble.DefaultConfig(),
		Trigger: assemble.Trigger{Kind: kind, Item: item},
		Now:     now,
	})
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: 组装失败: %v", err)
	}

	expiresAt := now.Add(contextBaseSnapshotTTL)
	resp := protocol.ContextResponse{
		Schema:       contextBaseSchema,
		SnapshotID:   spv2.NewID(),
		ExpiresAt:    expiresAt.UTC().Format(time.RFC3339Nano),
		Messages:     toProtocolMessages(out.Messages),
		Task:         task,
		Work:         workViewOf(work, task.ID, w),
		Degraded:     toProtocolDegraded(out.Degraded),
		Warnings:     out.Warnings,
		AssembledAt:  now.UTC().Format(time.RFC3339Nano),
		TriggerState: store.TriggerStateAssembled,
	}
	payload, err := protocol.EncodePayload(resp)
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: 序列化快照失败: %v", err)
	}
	err = s.cfg.Store.SaveSnapshot(ctx, resp.SnapshotID, work, kind, item, payload, contextBaseSchema, now, expiresAt)
	if errors.Is(err, store.ErrSnapshotConflict) {
		// 同一触发的并发首组装：别人先写成功了，沿用那份，不在这里推翻它。
		resp, loadErr := s.loadContextBaseSnapshot(ctx, work, kind, item, store.TriggerStateAssembled)
		if loadErr != nil {
			return nil, loadErr
		}
		// 顺手补上「快照已写、状态没写」的崩溃窗口：只把空/pending 推到 assembled，
		// 已是 completed 的绝不回退（完成记录优先）。
		if state, _, stErr := s.cfg.Store.GetTriggerState(ctx, work, kind, item); stErr == nil &&
			(state == "" || state == store.TriggerStatePending) {
			_ = s.cfg.Store.SetTriggerState(ctx, work, kind, item, store.TriggerStateAssembled, resp.SnapshotID)
		}
		return resp, nil
	}
	if errors.Is(err, store.ErrSnapshotTooLarge) {
		// 容量上限是显式失败（S-001 §4.4）：不截断投影、不静默丢弃，也不写触发状态。
		// 存储层错误已带 work / trigger 与字节数，这里只补线上错误码。
		return nil, v2Error(spv2.CodeInvalidArgument, "context_base: %v", err)
	}
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	if err := s.cfg.Store.SetTriggerState(ctx, work, kind, item, store.TriggerStateAssembled, resp.SnapshotID); err != nil {
		// 快照已落库（不可变事实）而状态没写上：返回快照仍然安全——下次触发会重新组装、
		// 被唯一约束拦住后沿用这份快照，不会产生第二份投影。
		return nil, v2Error(spv2.CodeInternal, "context_base: %v", err)
	}
	return resp, nil
}

// ---------- trigger_complete ----------

// triggerComplete 标记本轮触发完成（S-001 §3.1.1）：幂等；对 pending 调用必须 conflict。
func (s *Server) triggerComplete(ctx context.Context, body []byte, c caller) (any, error) {
	var req protocol.TriggerCompletePayload
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return nil, v2Error(spv2.CodeInvalidArgument, "trigger_complete body 无效: %v", err)
	}
	work := strings.TrimSpace(req.Work)
	kind := strings.TrimSpace(req.Trigger.Kind)
	item := strings.TrimSpace(req.Trigger.Item)
	if work == "" || kind == "" || item == "" {
		return nil, v2Error(spv2.CodeInvalidArgument, "trigger_complete: work / trigger.kind / trigger.item 必填")
	}
	if _, err := s.authorizeTargetWork(ctx, work, c.work, "trigger_complete", c.from); err != nil {
		return nil, err
	}
	exists, err := s.cfg.Store.TriggerItemExists(ctx, work, item)
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "trigger_complete: %v", err)
	}
	if !exists {
		return nil, v2Error(spv2.CodeInvalidArgument,
			"trigger_complete: trigger.item %s 未落库或不属于 work %s", item, work)
	}
	err = s.cfg.Store.CompleteTrigger(ctx, work, kind, item)
	if errors.Is(err, store.ErrTriggerConflict) {
		return nil, v2Error(spv2.CodeConflict, "trigger_complete: %v", err)
	}
	if err != nil {
		return nil, v2Error(spv2.CodeInternal, "trigger_complete: %v", err)
	}
	return protocol.TriggerCompleteResult{Completed: true}, nil
}

// ---------- 投影：store 行 → assemble / protocol 结构 ----------

// recordsFromMessageRows 把消息行按 assemble 的输入分类：
// tool_call / tool_result 各走配对块，其余（含 reasoning / spawn_return）走历史。
//
// 工具名、参数与 call_id 从 meta（JSON）里取：reasoner 的轨迹写入约定是
// {"tool": "...", "call_id": "...", "arguments": "..."}；老记录没有 arguments 时
// 退化成一个空对象（模型侧要求的 arguments 必须是可解析 JSON，不能是空串）。
//
// 不再为 work_id 为空的行伪造 visibility=task（R-014 T4）：查询只返回本 work 的行，
// 空 work 的历史不属于任何 work，默认不可见才是正确语义。
func recordsFromMessageRows(rows []store.MessageRow) (history, toolCalls, toolResults []assemble.Record) {
	for _, row := range rows {
		kind := row.Kind
		if kind == "" {
			kind = kindFromMessageRole(row.Role)
		}
		role := row.Role
		if role == "" {
			role = roleFromMessageKind(kind)
		}
		rec := assemble.Record{
			ItemID:     row.ItemID,
			Role:       role,
			Kind:       kind,
			Content:    row.Content,
			WorkID:     row.WorkID,
			Visibility: row.Visibility,
			Seq:        row.Seq,
		}
		switch kind {
		case "tool_call":
			rec.CallID = messageMetaString(row.Meta, "call_id")
			rec.Name = messageMetaString(row.Meta, "tool")
			if rec.Name == "" {
				rec.Name = messageMetaString(row.Meta, "name")
			}
			rec.Arguments = messageMetaString(row.Meta, "arguments")
			if rec.Arguments == "" {
				rec.Arguments = "{}"
			}
			toolCalls = append(toolCalls, rec)
		case "tool_result":
			rec.CallID = messageMetaString(row.Meta, "call_id")
			toolResults = append(toolResults, rec)
		default:
			history = append(history, rec)
		}
	}
	return history, toolCalls, toolResults
}

// kindFromMessageRole 在 kind 为空时按 role 推断（与 store.kindOfRole 同口径）。
func kindFromMessageRole(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool_result"
	case "system":
		return "system"
	default:
		return role
	}
}

// roleFromMessageKind 在 role 为空时按 kind 推断（spawn_return 按用户输入渲染）。
func roleFromMessageKind(kind string) string {
	switch kind {
	case "user", "spawn_return":
		return "user"
	case "assistant", "reasoning", "tool_call":
		return "assistant"
	case "tool_result":
		return "tool"
	case "system":
		return "system"
	default:
		return ""
	}
}

// messageMetaString 从 meta JSON 里取一个标量字段；对象/数组编码回 JSON 文本
// （工具参数本身就是 JSON，不能 fmt.Sprint 成 map[...]）。
func messageMetaString(meta, key string) string {
	if strings.TrimSpace(meta) == "" {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(meta), &m); err != nil {
		return ""
	}
	v, ok := m[key]
	if !ok || v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	if raw, err := json.Marshal(v); err == nil {
		return string(raw)
	}
	return fmt.Sprint(v)
}

// toProtocolMessages 把 assemble.Message 映射为线载荷里的 LLMMessage（字段名与协议一致）。
func toProtocolMessages(msgs []assemble.Message) []protocol.LLMMessage {
	out := make([]protocol.LLMMessage, 0, len(msgs))
	for _, m := range msgs {
		lm := protocol.LLMMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			lm.ToolCalls = append(lm.ToolCalls, protocol.LLMToolCall{
				ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments,
			})
		}
		out = append(out, lm)
	}
	return out
}

// toProtocolDegraded 原样透传降级状态（nil = 无降级）。
func toProtocolDegraded(d *assemble.Degraded) *protocol.Degraded {
	if d == nil {
		return nil
	}
	return &protocol.Degraded{Level: d.Level, Reason: d.Reason, Since: d.Since, RepairHint: d.RepairHint}
}

// workViewOf 把 work 行投影成响应里的 WorkView。
func workViewOf(workID, taskID string, w spv2.Work) protocol.WorkView {
	return protocol.WorkView{
		ID:        workID,
		Task:      taskID,
		Class:     w.Class,
		Status:    w.Status,
		Owner:     w.Owner.ID,
		Agent:     w.Agent.ID,
		Entry:     w.Entry.ID,
		Goal:      w.Goal,
		CreatedAt: w.CreatedAt,
		UpdatedAt: w.UpdatedAt,
	}
}
