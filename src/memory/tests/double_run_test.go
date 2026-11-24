package tests

// P1 双跑等价：同一份库数据下，旧 `context` 与新 `context_v2` 的 A 核心规范化投影必须一致
// （S-001 §3.1「A 切片的核心承诺」、§10「A 双跑等价的比较口径」）。
//
// 比较口径（严格按 S-001 §10）：
//   - 只比较 A 核心的规范化投影：messages / task / work / degraded / warnings；
//   - 排除：pending / task_blockers、时间戳、随机 ID（snapshot_id / assembled_at / expires_at）、
//     provenance；
//   - messages 只比较 role / content / tool_calls（id/name/arguments）/ tool_call_id / name；
//   - 断言消息顺序一致、tool 调用与结果配对一致；不要求新旧响应逐字节相等。
//
// 被排除的消息（显式记录，不是偷偷放宽断言）：
//  1. role=system：
//     - 旧 context 的 system 来自调用方托管的 prompt，原样回传（老契约把消息链托管给 reasoner）；
//     - 新 context_v2 的 system 由 memory 按 task 生成（render.go 的 taskPrompt，INV-1/INV-3）。
//     两者来源语义不同，属于迁移目标本身，按 A 核心双跑口径排除；system 条数与归属仍在下面
//     逐个断言（旧 1 条、新 1 条，且都不含【记忆】块）。
//  2. `【记忆】` 块：
//     - 旧 context 用 compose.Inject 把「库内、但不在调用方链尾部」的历史折叠成一条 system
//       记忆块（`【记忆】` 前缀）；
//     - 新 context_v2 把同一段历史作为独立的 user/assistant 消息输出，不折叠、不二次注入。
//     这是旧实现的折叠语义，**不能无条件忽略**：TestDoubleRunMemoryBlockDifference 里单独
//     断言「除这条记忆块外，其余非 system 消息逐条一致，且被折叠的内容确实全部出现在块里」。
//
// 已知的字段级差异（同样显式记录）：
//  3. degraded / warnings：旧 context 的响应载荷里根本没有这两个字段；本用例的库数据不会触发
//     降级或 warning，所以投影是「旧：未表达 = 新：nil / 空集」。测试同时断言新响应确实没有
//     降级、没有 warning——不是把非空差异当空放过。
//  4. work.task：旧 context 返回的是 spv2.Work，结构里没有 task 字段（task 在 works.task 列）；
//     新 context_v2 的 WorkView 有 task。测试用同一库行 works.task 给旧投影补齐该字段后再比较。
//  5. 时间戳 / 随机 ID：task.created_at|updated_at、work.created_at|updated_at、assembled_at、
//     snapshot_id、expires_at 一律排除（快照稳定性单独用「同 trigger → 同 snapshot_id」验证）。
//
// 离线、可重复：设置中心是 httptest 替身；消息写入与 context 调用都走真 WS + 真 SQLite
// （t.TempDir()）；假 LLM 是本地记录器，不联网、不依赖时间（不 sleep，过期靠改库里的
// expires_at 模拟）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

const (
	doubleRunWorkID   = "w-double-run"
	doubleRunConvID   = "conv-double-run"
	doubleRunConvName = "双跑等价会话"
	doubleRunTaskName = "双跑等价任务"
	doubleRunOwner    = "owner"
	doubleRunAgent    = "reasoner"
	doubleRunEntry    = "frontend"
	doubleRunGoal     = "验证新旧上下文装配等价"
)

// ---------- 库数据 ----------

// doubleRunRow 是测试要写进库的一条消息（Seq 是逻辑顺序；v2 路径下 seq 由 memory 分配）。
// Meta 只填工具记录需要的 call_id / tool / arguments（§10 要求带 meta.call_id）。
type doubleRunRow struct {
	Seq     int64
	Role    string
	Kind    string
	Content string
	Meta    string
}

// doubleRunRows 是本次双跑共用的消息链：user → assistant → user → tool_call → tool_result。
// tool 记录放在最后：assemble 的固定顺序是「历史 → 图片 → 工具配对」，这样旧 context 的
// 逐条透传顺序与新 context_v2 的装配顺序才谈得上一致（否则比的是两种不同的排序规则）。
//
// Seq 是这条消息**应有的逻辑顺序**：v2 路径下 seq 由 memory 的 seq_counters 在事务内分配
// （S-001 §3.0 规则 2），所以本用例按 Seq 升序写入，让分配出来的 seq 与逻辑顺序一致。
// 「乱序写入也要按 seq 读」这件事由 store/seq_order_test.go 与 server/context_v2_test.go
// 用直接写库的方式覆盖，不在这里靠调用方传 seq 伪造。
func doubleRunRows() []doubleRunRow {
	return []doubleRunRow{
		{Seq: 1, Role: "user", Kind: "user", Content: "帮我看下当前目录"},
		{Seq: 2, Role: "assistant", Kind: "assistant", Content: "好的，我来执行 pwd。"},
		{Seq: 3, Role: "user", Kind: "user", Content: "顺便确认一下现在是几点"},
		{
			Seq: 4, Role: "assistant", Kind: "tool_call", Content: "",
			Meta: doubleRunMeta(map[string]string{
				"tool": "shell__run", "call_id": "call-1", "arguments": `{"command":"pwd"}`,
			}),
		},
		{
			Seq: 5, Role: "tool", Kind: "tool_result", Content: `C:\work\demo`,
			Meta: doubleRunMeta(map[string]string{"call_id": "call-1"}),
		},
	}
}

// doubleRunMeta 把工具元信息拼成 store 要求的 meta JSON 字符串。
func doubleRunMeta(m map[string]string) string {
	raw, err := json.Marshal(m)
	if err != nil {
		panic(err) // 固定字面量，不可能失败
	}
	return string(raw)
}

// doubleRunFixture 是一份“同一份库数据”的完整现场：conversation + task + work + 消息。
type doubleRunFixture struct {
	t      *testing.T
	srv    *server.Server
	store  *store.Store
	taskID string
	rows   []doubleRunRow
	// itemIDs 是 seq → messages.id 的十进制字符串（v2 的 trigger.item 就是它）。
	itemIDs map[int64]string
	// workTask 是库内 works.task 的补投影（旧 context 的 work 载荷没有 task 字段）。
	workTask string
}

// newDoubleRunFixture 起真 memory 服务（临时库 + 设置中心替身），并造出 task / conversation /
// work 行与带 work_id、seq、meta.call_id 的消息。
func newDoubleRunFixture(t *testing.T) *doubleRunFixture {
	t.Helper()
	ctx := context.Background()

	sc := startStubCenter(t)
	var st *store.Store
	srv := startMemory(t, sc.URL(), 12, func(_ *server.Config, s *store.Store) { st = s })
	if st == nil {
		t.Fatal("没有拿到 memory 的库句柄")
	}

	// conversation（显式一行，避免“有消息没会话”的孤儿形态）。
	if err := st.EnsureConversation(doubleRunConvID, doubleRunConvName, doubleRunEntry); err != nil {
		t.Fatalf("登记会话失败: %v", err)
	}
	// task：进行中的任务（旧 context 的 EnsureTask 会复用它，不会另开一个）。
	task, err := st.EnsureTask(doubleRunConvID, doubleRunTaskName)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if task.Name != doubleRunTaskName || task.ConversationID != doubleRunConvID {
		t.Fatalf("任务行不符: %+v", task)
	}
	// work：id / root / class / owner / agent / entry / goal / status 全部显式给。
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID:     doubleRunWorkID,
		Root:   doubleRunWorkID,
		Class:  spv2.ClassRequest,
		Owner:  spv2.HumanRef(doubleRunOwner),
		Agent:  spv2.AgentRef(doubleRunAgent),
		Entry:  spv2.ServiceRef(doubleRunEntry),
		Goal:   doubleRunGoal,
		Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatalf("登记 work 失败: %v", err)
	}
	if err := st.LinkWorkTask(ctx, doubleRunWorkID, task.ID); err != nil {
		t.Fatalf("把 work 挂到任务失败: %v", err)
	}

	// 回读 work 行，确认造出来的就是用例声明的那一行（§10 的“同一份库数据”）。
	w, err := st.GetWork(ctx, doubleRunWorkID)
	if err != nil {
		t.Fatalf("回读 work 失败: %v", err)
	}
	if w.ID != doubleRunWorkID || w.Class != spv2.ClassRequest || w.Status != spv2.WorkRunning ||
		w.Owner.ID != doubleRunOwner || w.Agent.ID != doubleRunAgent ||
		w.Entry.ID != doubleRunEntry || w.Goal != doubleRunGoal {
		t.Fatalf("work 行不符: %+v", w)
	}
	_, workTask, err := st.WorkOwnerTask(ctx, doubleRunWorkID)
	if err != nil {
		t.Fatalf("回读 work 的 task 失败: %v", err)
	}
	if workTask != task.ID {
		t.Fatalf("works.task = %q，期望 %q", workTask, task.ID)
	}

	fx := &doubleRunFixture{
		t: t, srv: srv, store: st, taskID: task.ID,
		rows: doubleRunRows(), itemIDs: map[int64]string{}, workTask: workTask,
	}
	fx.insertMessages()
	fx.assertReadBack()
	return fx
}

// insertMessages 按逻辑顺序写入，并在载荷里带一个**明显错误的调用方 seq**：
// v2 路径必须忽略它、用 seq_counters 分配的值回执（S-001 §3.0 规则 2）。
func (fx *doubleRunFixture) insertMessages() {
	bySeq := make(map[int64]doubleRunRow, len(fx.rows))
	for _, row := range fx.rows {
		bySeq[row.Seq] = row
	}
	allocated := int64(0)
	for seq := int64(1); seq <= int64(len(fx.rows)); seq++ {
		row, ok := bySeq[seq]
		if !ok {
			fx.t.Fatalf("插入计划里的 seq=%d 没有对应消息", seq)
		}
		allocated++
		if got := fx.appendMessage(row, allocated); got == "" {
			fx.t.Fatalf("seq=%d 没有拿到 item_id", seq)
		}
	}
}

// appendMessage 走真 WS 的 store（v2 路径：带 client_item_id / work / task），返回 item id。
// wantSeq 是 memory 应当分配出来的 seq；载荷里刻意带上一个错误的调用方 seq，用来证明它不算数。
func (fx *doubleRunFixture) appendMessage(row doubleRunRow, wantSeq int64) string {
	fx.t.Helper()
	body, err := doubleRunCall(fx.t, fx.srv.WsURL(), doubleRunWorkID, doubleRunAgent, "store",
		protocol.StorePayload{
			ConversationID: doubleRunConvID,
			Work:           doubleRunWorkID,
			TaskID:         fx.taskID,
			Role:           row.Role,
			Kind:           row.Kind,
			Content:        row.Content,
			Meta:           row.Meta,
			Seq:            9000 + row.Seq, // 调用方给的 seq：v2 路径必须忽略
			ClientItemID:   fmt.Sprintf("double-run:%d", row.Seq),
		})
	if err != nil {
		fx.t.Fatalf("store（seq=%d）失败: %v", row.Seq, err)
	}
	var res protocol.StoreResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		fx.t.Fatalf("store（seq=%d）回执无法解码: %v", row.Seq, err)
	}
	if res.ItemID == "" || res.Seq != wantSeq {
		fx.t.Fatalf("store（seq=%d）回执不符: 期望 memory 分配 seq=%d，实际 %+v", row.Seq, wantSeq, res)
	}
	fx.itemIDs[row.Seq] = res.ItemID
	return res.ItemID
}

// assertReadBack 用 store 的读取路径核对“写进去的库数据”与用例声明逐条一致。
func (fx *doubleRunFixture) assertReadBack() {
	fx.t.Helper()
	rows, err := fx.store.RecentMessagesByWork(context.Background(), doubleRunWorkID, fx.taskID, 0)
	if err != nil {
		fx.t.Fatalf("回读 work 消息失败: %v", err)
	}
	if len(rows) != len(fx.rows) {
		fx.t.Fatalf("回读到 %d 条消息，期望 %d 条", len(rows), len(fx.rows))
	}
	for i, want := range fx.rows {
		got := rows[i]
		if got.ItemID != fx.itemIDs[want.Seq] || got.Seq != want.Seq ||
			got.Role != want.Role || got.Kind != want.Kind ||
			got.Content != want.Content || got.WorkID != doubleRunWorkID {
			fx.t.Fatalf("第 %d 条消息回读不符:\n got = %+v\nwant = %+v", i, got, want)
		}
	}
}

// legacyChain 把库内消息投影成旧 context 调用方持有的消息链（只取 seq >= fromSeq 的部分）。
// 这就是“同一份库数据下，reasoner 托管给旧 context 的那条链”——不是从新响应反推的。
func (fx *doubleRunFixture) legacyChain(fromSeq int64) []protocol.Message {
	out := make([]protocol.Message, 0, len(fx.rows))
	for _, row := range fx.rows {
		if row.Seq < fromSeq {
			continue
		}
		out = append(out, fx.legacyMessage(row))
	}
	return out
}

// legacyMessage 把一条库记录投影成旧协议的 protocol.Message（工具记录带 tool_calls / tool_call_id）。
func (fx *doubleRunFixture) legacyMessage(row doubleRunRow) protocol.Message {
	switch row.Kind {
	case "tool_call":
		var meta map[string]string
		if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
			fx.t.Fatalf("解析 seq=%d 的 meta 失败: %v", row.Seq, err)
		}
		return protocol.Message{
			Role: "assistant", Content: row.Content,
			ToolCalls: []protocol.ToolCall{{
				ID: meta["call_id"], Type: "function",
				Function: protocol.FunctionCall{Name: meta["tool"], Arguments: meta["arguments"]},
			}},
		}
	case "tool_result":
		var meta map[string]string
		if err := json.Unmarshal([]byte(row.Meta), &meta); err != nil {
			fx.t.Fatalf("解析 seq=%d 的 meta 失败: %v", row.Seq, err)
		}
		return protocol.Message{Role: "tool", Content: row.Content, ToolCallID: meta["call_id"]}
	default:
		return protocol.Message{Role: row.Role, Content: row.Content}
	}
}

// ---------- 两个入口的调用 ----------

// doubleRunLegacy 是旧 context 的回执（messages / work / task 三段，无 degraded / warnings）。
type doubleRunLegacy struct {
	Messages []protocol.Message `msgpack:"messages"`
	Work     spv2.Work          `msgpack:"work"`
	Task     protocol.Task      `msgpack:"task"`
}

// callLegacy 通过 WS 调旧 context：work 走帧头 work（旧路径按它登记/回读 work），
// Messages 是调用方托管的链（系统提示 + 库数据投影），trigger 概念在旧协议里不存在。
func (fx *doubleRunFixture) callLegacy(systemText string, fromSeq int64) doubleRunLegacy {
	fx.t.Helper()
	input := protocol.ContextPayload{
		Conversation: doubleRunConvID,
		Boss:         doubleRunEntry,
		TaskName:     doubleRunTaskName,
		Messages:     append([]protocol.Message{{Role: "system", Content: systemText}}, fx.legacyChain(fromSeq)...),
	}
	body, err := doubleRunCall(fx.t, fx.srv.WsURL(), doubleRunWorkID, doubleRunAgent, "context", input)
	if err != nil {
		fx.t.Fatalf("旧 context 失败: %v", err)
	}
	var resp doubleRunLegacy
	if err := protocol.DecodeRaw(body, &resp); err != nil {
		fx.t.Fatalf("旧 context 回执无法解码: %v", err)
	}
	return resp
}

// callV2 通过 WS 调新 context_v2；trigger.item 用一条已存在 user 消息的 id（十进制字符串）。
// 错误原样返回，供“过期 → not_found”用例判断错误码。
func (fx *doubleRunFixture) callV2(triggerSeq int64) (protocol.ContextResponse, error) {
	fx.t.Helper()
	item, ok := fx.itemIDs[triggerSeq]
	if !ok {
		fx.t.Fatalf("seq=%d 没有落库消息 id", triggerSeq)
	}
	// 契约要求 trigger.item 是 messages.id 的十进制字符串（§3.1 规则 1）。
	if _, err := strconv.ParseInt(item, 10, 64); err != nil {
		fx.t.Fatalf("trigger.item %q 不是十进制字符串: %v", item, err)
	}
	req := protocol.ContextRequest{
		Work:    doubleRunWorkID,
		Trigger: protocol.Trigger{Kind: "user", Item: item},
		Schema:  "context/2",
	}
	body, err := doubleRunCall(fx.t, fx.srv.WsURL(), doubleRunWorkID, doubleRunAgent, "context_v2", req)
	if err != nil {
		return protocol.ContextResponse{}, err
	}
	var resp protocol.ContextResponse
	if err := protocol.DecodeRaw(body, &resp); err != nil {
		fx.t.Fatalf("context_v2 回执无法解码: %v", err)
	}
	return resp, nil
}

// mustContextV2 是 callV2 的失败即终止版本。
func (fx *doubleRunFixture) mustContextV2(triggerSeq int64) protocol.ContextResponse {
	fx.t.Helper()
	resp, err := fx.callV2(triggerSeq)
	if err != nil {
		fx.t.Fatalf("context_v2（trigger seq=%d）失败: %v", triggerSeq, err)
	}
	return resp
}

// doubleRunCall 向 memory 发一次原生 call 并等回执；work 显式放进帧头（旧 context 依赖它）。
func doubleRunCall(t *testing.T, memoryURL, work, from, action string, input any) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := spv2.Dial(ctx, memoryURL)
	if err != nil {
		return nil, fmt.Errorf("连接 memory 失败: %w", err)
	}
	defer conn.Close()
	call, err := spv2.NewCall(work, spv2.ServiceRef(from), spv2.ServiceRef("memory"), action, input)
	if err != nil {
		return nil, fmt.Errorf("构造 %s call 失败: %w", action, err)
	}
	resp, err := conn.Call(ctx, call)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ---------- A 核心规范化投影 ----------

// normToolCall 是 tool_calls 的规范化视图（A 核心只比较 id / name / arguments）。
type normToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// normMessage 是一条消息的规范化视图（只保留 S-001 §10 指定的字段）。
type normMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []normToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
	Name       string         `json:"name,omitempty"`
}

// normTask 是 task 的规范化视图（时间戳排除）。
type normTask struct {
	ID             string `json:"id"`
	ConversationID string `json:"conversation_id"`
	Name           string `json:"name"`
	Status         string `json:"status"`
}

// normWork 是 work 的规范化视图（时间戳排除；Task 见文件头差异清单第 4 条）。
type normWork struct {
	ID     string `json:"id"`
	Task   string `json:"task"`
	Class  string `json:"class"`
	Status string `json:"status"`
	Owner  string `json:"owner"`
	Agent  string `json:"agent"`
	Entry  string `json:"entry,omitempty"`
	Goal   string `json:"goal,omitempty"`
}

// normDegraded 是 degraded 的规范化视图。
type normDegraded struct {
	Level      string `json:"level"`
	Reason     string `json:"reason"`
	Since      string `json:"since,omitempty"`
	RepairHint string `json:"repair_hint,omitempty"`
}

// coreProjection 是 A 核心规范化投影：不含时间戳 / 随机 ID / provenance / pending / task_blockers。
type coreProjection struct {
	Messages []normMessage `json:"messages"`
	Task     normTask      `json:"task"`
	Work     normWork      `json:"work"`
	Degraded *normDegraded `json:"degraded"`
	Warnings []string      `json:"warnings"`
}

// projectLegacy 把旧 context 回执投影成 A 核心。system 在消息投影里排除（见文件头第 1 条）；
// degraded / warnings 旧载荷没有字段，按“未表达 = 无”处理（见文件头第 3 条）。
func (fx *doubleRunFixture) projectLegacy(resp doubleRunLegacy) coreProjection {
	return coreProjection{
		Messages: coreMessagesFromProtocol(resp.Messages),
		Task: normTask{
			ID: resp.Task.ID, ConversationID: resp.Task.ConversationID,
			Name: resp.Task.Name, Status: resp.Task.Status,
		},
		Work: normWork{
			ID: resp.Work.ID, Task: fx.workTask, Class: resp.Work.Class,
			Status: resp.Work.Status, Owner: resp.Work.Owner.ID,
			Agent: resp.Work.Agent.ID, Entry: resp.Work.Entry.ID, Goal: resp.Work.Goal,
		},
		Degraded: nil,
		Warnings: []string{},
	}
}

// projectV2 把新 context_v2 回执投影成 A 核心（同样的 system 排除规则）。
func projectV2(resp protocol.ContextResponse) coreProjection {
	warnings := append([]string{}, resp.Warnings...)
	if warnings == nil {
		warnings = []string{}
	}
	var degraded *normDegraded
	if resp.Degraded != nil {
		degraded = &normDegraded{
			Level: resp.Degraded.Level, Reason: resp.Degraded.Reason,
			Since: resp.Degraded.Since, RepairHint: resp.Degraded.RepairHint,
		}
	}
	return coreProjection{
		Messages: coreMessagesFromLLM(resp.Messages),
		Task: normTask{
			ID: resp.Task.ID, ConversationID: resp.Task.ConversationID,
			Name: resp.Task.Name, Status: resp.Task.Status,
		},
		Work: normWork{
			ID: resp.Work.ID, Task: resp.Work.Task, Class: resp.Work.Class,
			Status: resp.Work.Status, Owner: resp.Work.Owner,
			Agent: resp.Work.Agent, Entry: resp.Work.Entry, Goal: resp.Work.Goal,
		},
		Degraded: degraded,
		Warnings: warnings,
	}
}

// coreMessagesFromProtocol 把旧协议消息投影成 A 核心 messages：system 排除（文件头第 1 条），
// 其余只留 role / content / tool_calls / tool_call_id（旧协议没有 name 字段 → 空，见差异清单）。
func coreMessagesFromProtocol(msgs []protocol.Message) []normMessage {
	var out []normMessage
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		nm := normMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			nm.ToolCalls = append(nm.ToolCalls, normToolCall{
				ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments,
			})
		}
		out = append(out, nm)
	}
	return out
}

// coreMessagesFromLLM 把新协议的 LLMMessage 投影成 A 核心 messages（同样的 system 排除规则）。
func coreMessagesFromLLM(msgs []protocol.LLMMessage) []normMessage {
	var out []normMessage
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		nm := normMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			nm.ToolCalls = append(nm.ToolCalls, normToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		out = append(out, nm)
	}
	return out
}

// systemMessagesOfProtocol / systemMessagesOfLLM 提取被排除的 system 消息内容，供显式断言使用。
func systemMessagesOfProtocol(msgs []protocol.Message) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

func systemMessagesOfLLM(msgs []protocol.LLMMessage) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

// doubleRunWantProjection 是独立于两端实现的期望投影（字面量，不从任何一端的响应反推）。
func doubleRunWantProjection(fx *doubleRunFixture) coreProjection {
	return coreProjection{
		Messages: []normMessage{
			{Role: "user", Content: "帮我看下当前目录"},
			{Role: "assistant", Content: "好的，我来执行 pwd。"},
			{Role: "user", Content: "顺便确认一下现在是几点"},
			{
				Role: "assistant",
				ToolCalls: []normToolCall{
					{ID: "call-1", Name: "shell__run", Arguments: `{"command":"pwd"}`},
				},
			},
			{Role: "tool", Content: `C:\work\demo`, ToolCallID: "call-1"},
		},
		Task: normTask{
			ID: fx.taskID, ConversationID: doubleRunConvID,
			Name: doubleRunTaskName, Status: store.TaskActive,
		},
		Work: normWork{
			ID: doubleRunWorkID, Task: fx.taskID, Class: spv2.ClassRequest,
			Status: spv2.WorkRunning, Owner: doubleRunOwner,
			Agent: doubleRunAgent, Entry: doubleRunEntry, Goal: doubleRunGoal,
		},
		Degraded: nil,
		Warnings: []string{},
	}
}

// ---------- 假 LLM（agent 原样转发的落点） ----------

// llmToolCall / llmMessage 是“LLM 服务侧收到的消息”的最小镜像。
// 测试不 import 其他服务的包（架构红线：服务之间只共享协议），所以在这里镜像一份 llm.Message。
type llmToolCall struct {
	ID, Name, Arguments string
}

type llmMessage struct {
	Role       string
	Content    string
	ToolCalls  []llmToolCall
	ToolCallID string
	Name       string
}

// doubleRunFakeLLM 是最小假模型：记录收到的消息链，返回固定文本（离线、可重复）。
// INV-1 / INV-2：agent 只把 memory 给的 messages 原样转发，不重新组装、不改字段。
type doubleRunFakeLLM struct {
	got []llmMessage
}

func (f *doubleRunFakeLLM) Complete(msgs []protocol.LLMMessage) string {
	f.got = f.got[:0]
	for _, m := range msgs {
		lm := llmMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			lm.ToolCalls = append(lm.ToolCalls, llmToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		f.got = append(f.got, lm)
	}
	return "（假模型：消息已收到）"
}

// normalizeLLMMessages 把 llm.Message 映射回 A 核心规范化视图（含 system，用于“完全一致”断言）。
func normalizeLLMMessages(msgs []llmMessage) []normMessage {
	out := make([]normMessage, 0, len(msgs))
	for _, m := range msgs {
		nm := normMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			nm.ToolCalls = append(nm.ToolCalls, normToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		out = append(out, nm)
	}
	return out
}

// allMessagesFromLLM 把 context_v2 的 messages 映射成含 system 的规范化视图。
func allMessagesFromLLM(msgs []protocol.LLMMessage) []normMessage {
	out := make([]normMessage, 0, len(msgs))
	for _, m := range msgs {
		nm := normMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID, Name: m.Name}
		for _, tc := range m.ToolCalls {
			nm.ToolCalls = append(nm.ToolCalls, normToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
		}
		out = append(out, nm)
	}
	return out
}

// dropSystemMessages 去掉 system（A 核心 messages 的口径，见文件头第 1 条）。
func dropSystemMessages(msgs []normMessage) []normMessage {
	out := make([]normMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		out = append(out, m)
	}
	return out
}

// ---------- 断言工具 ----------

// doubleRunJSON 是失败信息里的可读投影（不用于 equality 判定本身）。
func doubleRunJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(raw)
}

// assertProjectionEqual 断言规范化投影逐字段一致；失败时打出双方的 JSON（便于定位差异）。
func assertProjectionEqual(t *testing.T, label string, got, want coreProjection) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s 的 A 核心投影不一致:\n got = %s\nwant = %s", label, doubleRunJSON(t, got), doubleRunJSON(t, want))
	}
}

// assertOrderAndToolPairing 显式钉住两条 A 核心事实（不只依赖上面的全等断言）：
//   - 消息顺序：历史按 seq 升序，随后是工具配对块；
//   - 配对：assistant.tool_calls 后面紧跟 role=tool，call_id 一致，tool_call 内容留空。
func assertOrderAndToolPairing(t *testing.T, label string, msgs []normMessage) {
	t.Helper()
	wantContents := []string{
		"帮我看下当前目录",
		"好的，我来执行 pwd。",
		"顺便确认一下现在是几点",
		"",
		`C:\work\demo`,
	}
	if len(msgs) != len(wantContents) {
		t.Fatalf("%s: 消息数 = %d，期望 %d: %+v", label, len(msgs), len(wantContents), msgs)
	}
	for i, want := range wantContents {
		if msgs[i].Content != want {
			t.Fatalf("%s: 第 %d 条消息顺序不符: got %q, want %q（全部: %+v）", label, i, msgs[i].Content, want, msgs)
		}
	}

	callIdx := -1
	for i, m := range msgs {
		if len(m.ToolCalls) > 0 {
			if callIdx >= 0 {
				t.Fatalf("%s: 只应有一次工具调用，第 %d 与 %d 条都有: %+v", label, callIdx, i, msgs)
			}
			callIdx = i
		}
	}
	if callIdx < 0 || callIdx+1 >= len(msgs) {
		t.Fatalf("%s: 没有找到 assistant.tool_calls + role=tool 的配对: %+v", label, msgs)
	}
	call, pair := msgs[callIdx], msgs[callIdx+1]
	if call.Role != "assistant" || len(call.ToolCalls) != 1 || call.ToolCalls[0].ID != "call-1" ||
		call.ToolCalls[0].Name != "shell__run" || call.ToolCalls[0].Arguments != `{"command":"pwd"}` {
		t.Fatalf("%s: tool_call 字段不符（§10 要求 tool 配对一致）: %+v", label, call)
	}
	if pair.Role != "tool" || pair.ToolCallID != "call-1" || pair.Content != `C:\work\demo` {
		t.Fatalf("%s: tool 结果与调用配对不对: %+v", label, pair)
	}
}

// ---------- 主用例 ----------

// TestDoubleRunContextEquivalence 是 A 核心双跑等价的主用例：
// 同一份库数据 → 旧 context（调用方托管链）与新 context_v2（库内装配）的核心投影必须一致。
func TestDoubleRunContextEquivalence(t *testing.T) {
	fx := newDoubleRunFixture(t)

	// 双跑：两端读的是同一份库数据；旧入口额外需要调用方托管的 system prompt 与消息链。
	legacy := fx.callLegacy("sys", 1)
	newResp := fx.mustContextV2(1)

	t.Run("A核心规范化投影", func(t *testing.T) {
		want := doubleRunWantProjection(fx)

		// 被排除的 system 消息：显式记录并断言它们的来源，而不是静默丢掉。
		legacySystems := systemMessagesOfProtocol(legacy.Messages)
		if len(legacySystems) != 1 || legacySystems[0] != "sys" {
			t.Fatalf("旧 context 的 system 应只有调用方托管的 1 条 \"sys\"，实际: %+v", legacySystems)
		}
		newSystems := systemMessagesOfLLM(newResp.Messages)
		if len(newSystems) != 1 {
			t.Fatalf("新 context_v2 的 system 应只有 1 条（任务提示），实际: %+v", newSystems)
		}
		if !strings.Contains(newSystems[0], doubleRunTaskName) || !strings.Contains(newSystems[0], "当前任务") {
			t.Fatalf("新 context_v2 的 system 应是由 task 生成的任务提示，实际: %q", newSystems[0])
		}
		// 本用例的调用方链完整覆盖了库内历史（compose.TrimOverlap 全部去重），旧入口不得再注入
		// 【记忆】块；新入口也不做记忆块注入（v2 原型未接 Memories）。这一点先钉死，
		// 「旧入口折叠历史成记忆块」的差异留给 TestDoubleRunMemoryBlockDifference 显式归类。
		for _, sys := range append(append([]string{}, legacySystems...), newSystems...) {
			if strings.Contains(sys, "【记忆】") {
				t.Fatalf("本用例不应出现【记忆】块（调用方链已完整覆盖库内历史）: %q", sys)
			}
		}

		legacyProj := fx.projectLegacy(legacy)
		newProj := projectV2(newResp)

		// 两端都必须等于独立写下的期望投影（防止“两边一起错”也能过）。
		assertProjectionEqual(t, "旧 context", legacyProj, want)
		assertProjectionEqual(t, "新 context_v2", newProj, want)
		if !reflect.DeepEqual(legacyProj, newProj) {
			t.Fatalf("新旧 A 核心投影不一致:\n old = %s\n new = %s",
				doubleRunJSON(t, legacyProj), doubleRunJSON(t, newProj))
		}

		// 顺序与 tool 配对（§10：另行验证消息顺序、tool 配对）。
		assertOrderAndToolPairing(t, "旧 context", legacyProj.Messages)
		assertOrderAndToolPairing(t, "新 context_v2", newProj.Messages)

		// degraded / warnings：旧载荷没有这两个字段；本次数据也不该触发降级或 warning。
		if newResp.Degraded != nil {
			t.Fatalf("本次双跑不应降级，实际 degraded = %+v", newResp.Degraded)
		}
		if len(newResp.Warnings) != 0 {
			t.Fatalf("本次双跑不应产生 warning，实际 = %+v", newResp.Warnings)
		}

		// snapshot_id / expires_at / assembled_at 属于随机 ID / 时间戳，按 §10 排除；
		// 只做“存在且形态正确”的独立断言，不参与新旧比较。
		if newResp.SnapshotID == "" {
			t.Fatal("context_v2 应返回 snapshot_id")
		}
		if expires, err := time.Parse(time.RFC3339Nano, newResp.ExpiresAt); err != nil || !expires.After(time.Now()) {
			t.Fatalf("expires_at 应在将来: %q err=%v", newResp.ExpiresAt, err)
		}
		if newResp.AssembledAt == "" {
			t.Fatal("context_v2 应返回 assembled_at")
		}
	})

	t.Run("fakeLLM收到的messages与context_v2一致", func(t *testing.T) {
		// agent 原样转发 context_v2 的 messages 给 LLM：模型看到的必须就是 memory 给的。
		llm := &doubleRunFakeLLM{}
		llm.Complete(newResp.Messages)

		gotForwarded := normalizeLLMMessages(llm.got)
		wantForwarded := allMessagesFromLLM(newResp.Messages)
		if !reflect.DeepEqual(gotForwarded, wantForwarded) {
			t.Fatalf("fake LLM 收到的 messages 与 context_v2 响应不一致:\n llm = %s\nresp = %s",
				doubleRunJSON(t, gotForwarded), doubleRunJSON(t, wantForwarded))
		}

		// 去掉 system 后，模型看到的消息 == A 核心 messages == 旧 context 的 A 核心 messages。
		want := doubleRunWantProjection(fx)
		llmCore := dropSystemMessages(gotForwarded)
		if !reflect.DeepEqual(llmCore, want.Messages) {
			t.Fatalf("fake LLM 的 A 核心 messages 不符:\n got = %s\nwant = %s",
				doubleRunJSON(t, llmCore), doubleRunJSON(t, want.Messages))
		}
		legacyCore := fx.projectLegacy(legacy).Messages
		if !reflect.DeepEqual(llmCore, legacyCore) {
			t.Fatalf("fake LLM 的 messages 与旧 context 不一致:\n llm = %s\n old = %s",
				doubleRunJSON(t, llmCore), doubleRunJSON(t, legacyCore))
		}
	})

	t.Run("快照稳定性", func(t *testing.T) {
		// 同一 trigger 再调一次：assembled 分支必须沿用同一快照。
		second := fx.mustContextV2(1)
		if second.SnapshotID != newResp.SnapshotID {
			t.Fatalf("同一 trigger 第二次的 snapshot_id 变了: %q vs %q", second.SnapshotID, newResp.SnapshotID)
		}
		firstJSON, err := json.Marshal(newResp.Messages)
		if err != nil {
			t.Fatal(err)
		}
		secondJSON, err := json.Marshal(second.Messages)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstJSON, secondJSON) {
			t.Fatalf("同一 trigger 的 messages 不是逐字节相同:\n first = %s\nsecond = %s", firstJSON, secondJSON)
		}

		// 快照生成后再往库里写一条消息：仍不得重新组装（快照是不可变投影）。
		fx.appendMessage(doubleRunRow{Seq: 6, Role: "user", Kind: "user", Content: "快照生成后才写入的消息"}, 6)
		third := fx.mustContextV2(1)
		if third.SnapshotID != newResp.SnapshotID {
			t.Fatalf("库内新增消息后 snapshot_id 变了（被重新组装）: %q vs %q", third.SnapshotID, newResp.SnapshotID)
		}
		thirdJSON, err := json.Marshal(third.Messages)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(firstJSON, thirdJSON) {
			t.Fatalf("库内新增消息后 messages 变了（被重新组装）:\n first = %s\n third = %s", firstJSON, thirdJSON)
		}
	})

	t.Run("快照过期not_found不重建", func(t *testing.T) {
		item, ok := fx.itemIDs[1]
		if !ok {
			t.Fatal("缺少 trigger 消息 id")
		}
		// 把快照 expires_at 改到过去（模拟 24h 保留期到期；不 sleep、不依赖时钟）。
		past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
		res, err := fx.store.DB().Exec(
			`UPDATE context_snapshots SET expires_at = ? WHERE work = ? AND trigger_kind = ? AND trigger_item = ?`,
			past, doubleRunWorkID, "user", item)
		if err != nil {
			t.Fatalf("改快照过期时间失败: %v", err)
		}
		if n, _ := res.RowsAffected(); n != 1 {
			t.Fatalf("应改到 1 行快照，实际 %d", n)
		}

		// assembled 状态 + 快照过期 → not_found（§3.1 规则 2 expired 行：不得静默重建）。
		if _, err := fx.callV2(1); !spv2.IsCode(err, spv2.CodeNotFound) {
			t.Fatalf("快照过期后 context_v2 应返回 not_found，实际: %v", err)
		}

		// 不重建：快照行数不变、触发状态仍是 assembled、snapshot_id 没被换掉。
		var snapshots int
		if err := fx.store.DB().QueryRow(
			`SELECT COUNT(*) FROM context_snapshots WHERE work = ?`, doubleRunWorkID).Scan(&snapshots); err != nil {
			t.Fatalf("统计快照失败: %v", err)
		}
		if snapshots != 1 {
			t.Fatalf("过期后不应重建快照，实际快照数 %d", snapshots)
		}
		state, snapshotID, err := fx.store.GetTriggerState(context.Background(), doubleRunWorkID, "user", item)
		if err != nil {
			t.Fatalf("读取触发状态失败: %v", err)
		}
		if state != store.TriggerStateAssembled || snapshotID != newResp.SnapshotID {
			t.Fatalf("过期后触发状态不应被改写: state=%q snapshot=%q（期望 assembled / %q）",
				state, snapshotID, newResp.SnapshotID)
		}
	})
}

// TestDoubleRunMemoryBlockDifference 显式归类「旧 context 注入【记忆】块」这一语义差异：
// 当调用方只托管本轮尾部（库内更早的对话不在链上）时，旧入口会把那段历史折叠成一条
// `【记忆】` system 块；新入口把同一段历史作为独立 user/assistant 消息输出。
//
// 这里**不放宽主用例的断言**，而是把差异拆到可核对的最小事实：
//  1. 旧：system 恰好两条 —— 调用方 prompt + 【记忆】块（后者紧跟在第一条 system 之后）；
//  2. 新：system 恰好一条（任务提示），不含【记忆】块，也不折叠历史；
//  3. 除这条记忆块外一致：旧的非 system 链 == 新的非 system 链的严格尾部（逐条、同序）；
//  4. 被折叠掉的那一段（新的前缀）必须逐条出现在【记忆】块文本里（是折叠，不是丢弃）；
//  5. task / work / degraded / warnings 仍与主用例期望一致（差异只发生在 messages 的
//     system 形态与历史折叠，不影响其它 A 核心字段）。
func TestDoubleRunMemoryBlockDifference(t *testing.T) {
	fx := newDoubleRunFixture(t)

	// 调用方只托管 seq>=3 的尾部：seq1/seq2 在库内但不在链上 → 旧入口折叠进【记忆】块。
	partial := fx.callLegacy("sys", 3)
	full := fx.mustContextV2(1)

	legacySystems := systemMessagesOfProtocol(partial.Messages)
	if len(legacySystems) != 2 {
		t.Fatalf("旧 context 应产生「调用方 system + 【记忆】块」两条 system，实际: %+v", legacySystems)
	}
	if legacySystems[0] != "sys" {
		t.Fatalf("第一条 system 应是调用方 prompt，实际: %q", legacySystems[0])
	}
	memoryBlock := legacySystems[1]
	if !strings.HasPrefix(memoryBlock, "【记忆】") {
		t.Fatalf("第二条 system 应是【记忆】块，实际: %q", memoryBlock)
	}
	// 位置：紧跟在第一条 system 之后（compose.Inject 的语义），不是塞到队尾。
	if partial.Messages[1].Role != "system" || !strings.HasPrefix(partial.Messages[1].Content, "【记忆】") {
		t.Fatalf("【记忆】块应紧跟第一条 system，实际第 2 条 = %+v", partial.Messages[1])
	}

	newSystems := systemMessagesOfLLM(full.Messages)
	if len(newSystems) != 1 {
		t.Fatalf("新 context_v2 应只有 1 条 system（任务提示），实际: %+v", newSystems)
	}
	if strings.Contains(newSystems[0], "【记忆】") {
		t.Fatalf("新 context_v2 不应注入【记忆】块: %q", newSystems[0])
	}

	legacyCore := coreMessagesFromProtocol(partial.Messages)
	newCore := coreMessagesFromLLM(full.Messages)
	if len(legacyCore) > len(newCore) {
		t.Fatalf("旧链的非 system 消息比新链还多: old=%d new=%d", len(legacyCore), len(newCore))
	}
	folded := len(newCore) - len(legacyCore)

	// “除这些块外一致”：旧的非 system 链 == 新链的严格尾部，逐条同序（不是集合相等）。
	if !reflect.DeepEqual(legacyCore, newCore[folded:]) {
		t.Fatalf("除【记忆】块外，新旧非 system 消息不一致:\n old = %s\n new[%d:] = %s",
			doubleRunJSON(t, legacyCore), folded, doubleRunJSON(t, newCore[folded:]))
	}
	// 旧的尾部也必须等于期望投影的尾部（独立期望，防止两端一起错）。
	want := doubleRunWantProjection(fx)
	if !reflect.DeepEqual(legacyCore, want.Messages[folded:]) {
		t.Fatalf("旧链尾部与期望投影不一致:\n got = %s\nwant = %s",
			doubleRunJSON(t, legacyCore), doubleRunJSON(t, want.Messages[folded:]))
	}
	// 被折叠的新前缀必须逐条出现在【记忆】块里（折叠 ≠ 丢弃）。
	if folded == 0 {
		t.Fatalf("本用例应至少折叠 1 条历史，实际 folded=0（调用方链覆盖了全部历史？）")
	}
	for _, m := range newCore[:folded] {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if !strings.Contains(memoryBlock, m.Content) {
			t.Fatalf("被折叠的消息没有出现在【记忆】块里（内容丢失）: role=%s content=%q\nblock = %q",
				m.Role, m.Content, memoryBlock)
		}
	}

	// 差异只发生在 messages 的 system 形态 / 历史折叠上：task / work 仍一致。
	legacyProj := fx.projectLegacy(partial)
	newProj := projectV2(full)
	if !reflect.DeepEqual(legacyProj.Task, want.Task) {
		t.Fatalf("旧 context 的 task 投影与期望不一致: got %s want %s",
			doubleRunJSON(t, legacyProj.Task), doubleRunJSON(t, want.Task))
	}
	if !reflect.DeepEqual(newProj.Task, want.Task) {
		t.Fatalf("新 context_v2 的 task 投影与期望不一致: got %s want %s",
			doubleRunJSON(t, newProj.Task), doubleRunJSON(t, want.Task))
	}
	if !reflect.DeepEqual(legacyProj.Work, want.Work) {
		t.Fatalf("旧 context 的 work 投影与期望不一致: got %s want %s",
			doubleRunJSON(t, legacyProj.Work), doubleRunJSON(t, want.Work))
	}
	if !reflect.DeepEqual(newProj.Work, want.Work) {
		t.Fatalf("新 context_v2 的 work 投影与期望不一致: got %s want %s",
			doubleRunJSON(t, newProj.Work), doubleRunJSON(t, want.Work))
	}
	if legacyProj.Degraded != nil || len(legacyProj.Warnings) != 0 {
		t.Fatalf("旧 context 的降级/warning 投影应为空: %+v", legacyProj)
	}
	if newProj.Degraded != nil || len(newProj.Warnings) != 0 {
		t.Fatalf("新 context_v2 的降级/warning 投影应为空: %+v", newProj)
	}
}
