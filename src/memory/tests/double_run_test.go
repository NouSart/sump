package tests

// R-026 P2b 双跑等价（含 role=system）：同一份库数据下，旧 `context`（facade 现场组合
// 调用方托管链 + experience）与 `context_v2`（session 基座组装）在「切换后的最终模型链」
// 上必须逐条一致（S-001 §3.1 / §10；R-026-D1 / D3 / §5）。
//
// 比较口径（相对源 af6cfc8:src/memory/tests/double_run_test.go 的扩展）：
//   - 旧侧：`context` 回执 + reasoner 的 withTaskPrompt——任务提示插到调用方链的索引 1；
//   - 新侧：`context_v2` 回执 + 在链首 prepend 同一调用方 system；
//   - 比较：system 条数 / 内容 / 位置，非 system 链逐字相等，task / work / degraded / warnings；
//   - 排除：pending / task_blockers、时间戳、随机 ID（snapshot_id / assembled_at / expires_at）。
//
// 为什么这次 system 要比较（源测试排除它的理由已经不成立）：
//   - 切换后 system 由 memory 生成：`context_v2` 唯一的 system 是 memory 的 taskPrompt；
//   - reasoner 的 withTaskPrompt 文字与 memory assemble 的 taskPrompt 同文
//     （src/reasoner/server/infer.go:56-61 与 src/memory/session/assemble/render.go:9-22），
//     所以「调用方 system + taskPrompt」这条最终模型链必须逐字可比，而不是来源不同就跳过。
//   本文件没有 import reasoner（架构红线：服务之间只共享协议），因此把 withTaskPrompt 的
//   插入位置与 taskPrompt 文字镜像在测试里；新增的 `system纳入的最终模型链` 断言会把
//   memory 侧的任何文字漂移暴露成红灯（而不是让两端一起漂）。
//
// 显式记录的差异（不静默放宽断言）：
//  1. 折叠历史：旧 `context` 把「库内、但不在调用方链尾部」的历史折叠成一条 `【记忆】`
//     system 块（facade/compose.Inject）；`context_v2` 把同一段历史作为独立 user/assistant
//     消息输出。主用例的调用方链完整覆盖历史，所以不出现该块；折叠行为由
//     TestDoubleRunMemoryBlockDifference 单独断言：差异点恰好是「多一条 system 折叠块 +
//     新链多出被折叠的非 system 前缀」，非 system 尾部逐条一致。
//  2. degraded / warnings：旧 `context` 载荷没有这两个字段（按「未表达 = 无」投影）；本用例
//     数据不触发降级或 warning，测试同时断言新响应确实为空，而不是把非空差异当空放过。
//  3. work.task：旧载荷 spv2.Work 没有 task 字段；用 works.task（TaskIDOfWork）补齐后比较。
//  4. experience 段（R-026 §5）：本测试栈的 experience 为空，因此只断言「旧入口确实发起了
//     inject 调用（指标可见）且注入结果为空（没有经验线头）」，不做内容等价——内容等价属于
//     experience 自己的测试；`context_v2` 不投影 experience（R-026-D1）。
//  5. 源测试的 fakeLLM 子用例不移植：它验证的是 agent 把 context_v2 的 messages 原样转发，
//     那是 reasoner 的职责（本包不 import reasoner）；本测试的「最终模型链」已经断言模型
//     应看到的完整链（含 system），转发行为由 reasoner 自己的测试覆盖。
//  6. 快照稳定性 / 过期 not_found 两个子用例从源测试保留：它们走真实门面转发到 session，
//     是 context_v2 四态的端到端证据；随机 ID 与时间戳仍不参与新旧比较。
//
// 离线、可重复：设置中心是 httptest 替身；消息写入与两个 context 调用都走真 WS + 真 SQLite
// （t.TempDir()）；过期靠直接改库里的 expires_at 模拟，不依赖时钟等待。

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

	"github.com/NouSart/sump/memory/session/store"
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

	// doubleRunCallerSystem 是调用方托管的系统提示。切换后它仍由调用方给 reasoner，
	// 只是 memory 自己再生成一条 taskPrompt；两条 system 的顺序是 [调用方, taskPrompt]。
	doubleRunCallerSystem = "sys"
	// doubleRunMemoryPrefix 与 facade/compose.MemoryPrefix 同字面量。这里只用于识别折叠块，
	// 不参与拼接（拼接是生产代码的事）。
	doubleRunMemoryPrefix = "【记忆】"
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
// 「乱序写入也要按 seq 读」这件事由 store/seq_order_test.go 与 session/context_v2_test.go
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
// 装配复用 service_test.go 的三进程 testStack（门面 + session + experience），不另起一套。
type doubleRunFixture struct {
	t        *testing.T
	srv      *testStack
	store    *store.Store
	taskID   string
	taskName string
	rows     []doubleRunRow
	// itemIDs 是 seq → messages.id 的十进制字符串（v2 的 trigger.item 就是它）。
	itemIDs map[int64]string
}

// newDoubleRunFixture 起三进程栈（临时库 + 设置中心替身），并造出 task / conversation /
// work 行与带 work_id、seq、meta.call_id 的消息。
func newDoubleRunFixture(t *testing.T) *doubleRunFixture {
	t.Helper()
	ctx := context.Background()

	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	if srv.st == nil {
		t.Fatal("三进程栈没有暴露会话库句柄")
	}
	st := srv.st

	// conversation（显式一行，避免“有消息没会话”的孤儿形态）。
	if err := st.EnsureConversation(doubleRunConvID, doubleRunConvName, doubleRunEntry); err != nil {
		t.Fatalf("登记会话失败: %v", err)
	}
	// task：进行中的任务（context 的 EnsureTask 会复用它，不会另开一个）。
	task, err := st.EnsureTask(doubleRunConvID, doubleRunTaskName)
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if task.Name != doubleRunTaskName || task.ConversationID != doubleRunConvID || task.Status != store.TaskActive {
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
	workTask, err := st.TaskIDOfWork(ctx, doubleRunWorkID)
	if err != nil {
		t.Fatalf("回读 work 的 task 失败: %v", err)
	}
	if workTask != task.ID {
		t.Fatalf("works.task = %q，期望 %q", workTask, task.ID)
	}

	fx := &doubleRunFixture{
		t: t, srv: srv, store: st, taskID: task.ID, taskName: task.Name,
		rows: doubleRunRows(), itemIDs: map[int64]string{},
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
	rows, err := fx.store.RecentMessagesByWork(context.Background(), doubleRunWorkID, 0)
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

// callLegacy 通过 WS 调旧 context：work 走帧头 work，Messages 是调用方托管的链
// （系统提示 + 库数据投影），trigger 概念在旧协议里不存在。
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

// callV2 通过 WS 调 context_v2（门面转发到 session）；trigger.item 用一条已存在 user
// 消息的 id（十进制字符串）。错误原样返回，供“过期 → not_found”用例判断错误码。
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

// doubleRunCall 向 memory 发一次原生 call 并等回执；work 显式放进帧头。
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

// ---------- 规范化投影 ----------

// normToolCall 是 tool_calls 的规范化视图（A 核心只比较 id / name / arguments）。
type normToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// normMessage 是一条消息的规范化视图。
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

// normWork 是 work 的规范化视图（时间戳排除；Task 见文件头差异清单第 3 条）。
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

// doubleRunProjection 是“切换后模型链 + A 核心”的规范化投影：
// Chain 含 system（这次的要求），其余字段不含时间戳 / 随机 ID / provenance。
type doubleRunProjection struct {
	Chain    []normMessage `json:"chain"`
	Task     normTask      `json:"task"`
	Work     normWork      `json:"work"`
	Degraded *normDegraded `json:"degraded"`
	Warnings []string      `json:"warnings"`
}

// doubleRunTaskPrompt 是 reasoner/server/infer.go:56-61 的 taskPrompt 文字的镜像。
// 不 import reasoner（架构红线）；memory/session/assemble/render.go 的 taskPrompt 必须与它同文，
// 否则「新侧 system 内容 == 旧侧插入的 taskPrompt」这条断言会红。
func doubleRunTaskPrompt(name, id string) string {
	return fmt.Sprintf("当前任务：《%s》（id=%s）。你的所有动作都属于这个任务；换任务时上下文会重新开始。", name, id)
}

// doubleRunWithTaskPrompt 镜像 reasoner 的 withTaskPrompt（infer.go:63-66）：
// 第 0 条原样保留，任务提示插在索引 1，其余依次后移。
func doubleRunWithTaskPrompt(msgs []normMessage, name, id string) []normMessage {
	if len(msgs) == 0 {
		panic("withTaskPrompt: 空消息链")
	}
	out := make([]normMessage, 0, len(msgs)+1)
	out = append(out, msgs[0])
	out = append(out, normMessage{Role: "system", Content: doubleRunTaskPrompt(name, id)})
	out = append(out, msgs[1:]...)
	return out
}

// legacyModelChain 模拟旧链路的最终模型输入：旧 context 回执 → withTaskPrompt。
func (fx *doubleRunFixture) legacyModelChain(resp doubleRunLegacy) []normMessage {
	return doubleRunWithTaskPrompt(doubleRunMessagesFromProtocol(resp.Messages), fx.taskName, fx.taskID)
}

// newModelChain 模拟切换后的最终模型输入：context_v2 回执前 prepend 调用方 system。
// （切换后 reasoner 不再自己插 taskPrompt；taskPrompt 已是 memory 返回的第一条 system。）
func doubleRunNewModelChain(resp protocol.ContextResponse) []normMessage {
	out := make([]normMessage, 0, len(resp.Messages)+1)
	out = append(out, normMessage{Role: "system", Content: doubleRunCallerSystem})
	out = append(out, doubleRunMessagesFromLLM(resp.Messages)...)
	return out
}

// doubleRunMessagesFromProtocol 把旧协议消息投影成规范化视图（**保留 system**）。
func doubleRunMessagesFromProtocol(msgs []protocol.Message) []normMessage {
	out := make([]normMessage, 0, len(msgs))
	for _, m := range msgs {
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

// doubleRunMessagesFromLLM 把新协议的 LLMMessage 投影成规范化视图（**保留 system**）。
func doubleRunMessagesFromLLM(msgs []protocol.LLMMessage) []normMessage {
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

// dropSystemMessages 去掉 system（只用于“非 system 链”这一半的比较）。
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

// doubleRunSystemIndexes 返回 system 消息在链里的下标（按出现顺序）。
func doubleRunSystemIndexes(msgs []normMessage) []int {
	var out []int
	for i, m := range msgs {
		if m.Role == "system" {
			out = append(out, i)
		}
	}
	return out
}

// doubleRunSystemContents 返回 system 消息内容（按出现顺序）。
func doubleRunSystemContents(msgs []normMessage) []string {
	var out []string
	for _, m := range msgs {
		if m.Role == "system" {
			out = append(out, m.Content)
		}
	}
	return out
}

// projectLegacyFinal 把旧 context 的**最终模型链**投影成比较结构。
func (fx *doubleRunFixture) projectLegacyFinal(resp doubleRunLegacy) doubleRunProjection {
	return doubleRunProjection{
		Chain: fx.legacyModelChain(resp),
		Task: normTask{
			ID: resp.Task.ID, ConversationID: resp.Task.ConversationID,
			Name: resp.Task.Name, Status: resp.Task.Status,
		},
		Work: normWork{
			ID: resp.Work.ID, Task: fx.taskID, Class: resp.Work.Class,
			Status: resp.Work.Status, Owner: resp.Work.Owner.ID,
			Agent: resp.Work.Agent.ID, Entry: resp.Work.Entry.ID, Goal: resp.Work.Goal,
		},
		Degraded: nil, // 旧载荷没有该字段：未表达 = 无（文件头差异第 2 条）
		Warnings: []string{},
	}
}

// projectV2Final 把新 context_v2 的**最终模型链**投影成比较结构。
func projectV2Final(resp protocol.ContextResponse) doubleRunProjection {
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
	return doubleRunProjection{
		Chain: doubleRunNewModelChain(resp),
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

// doubleRunWantProjection 是独立于两端实现的期望投影（字面量，不从任何一端的响应反推）。
// Chain 按切换后的最终模型链写：调用方 system → taskPrompt → 库内历史 → 工具配对。
func doubleRunWantProjection(fx *doubleRunFixture) doubleRunProjection {
	return doubleRunProjection{
		Chain: []normMessage{
			{Role: "system", Content: doubleRunCallerSystem},
			{Role: "system", Content: doubleRunTaskPrompt(fx.taskName, fx.taskID)},
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

// assertSystemLayout 显式钉住 system 的条数 / 内容 / 位置：
// 期望恰好是 want 里的这几条，且按顺序占据链首（这是切换后模型链的硬形状）。
func assertSystemLayout(t *testing.T, label string, msgs []normMessage, want []string) {
	t.Helper()
	got := doubleRunSystemContents(msgs)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: system 内容/条数不符:\n got = %+v\nwant = %+v（完整链: %s）",
			label, got, want, doubleRunJSON(t, msgs))
	}
	idx := doubleRunSystemIndexes(msgs)
	wantIdx := make([]int, len(want))
	for i := range wantIdx {
		wantIdx[i] = i
	}
	if !reflect.DeepEqual(idx, wantIdx) {
		t.Fatalf("%s: system 位置不符（必须按顺序在链首）: got %v, want %v（完整链: %s）",
			label, idx, wantIdx, doubleRunJSON(t, msgs))
	}
}

// assertProjectionEqual 断言规范化投影逐字段一致；失败时打出双方的 JSON（便于定位差异）。
func assertProjectionEqual(t *testing.T, label string, got, want doubleRunProjection) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s 的投影不一致:\n got = %s\nwant = %s", label, doubleRunJSON(t, got), doubleRunJSON(t, want))
	}
}

// assertOrderAndToolPairing 显式钉住两条核心事实（不只依赖全等断言）：
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

// doubleRunWaitRequest 等到聚合指标里出现一次成功的 method 请求。
// 用途：证明旧 context 确实发起了 experience.inject（R-026 §5 的「存在注入调用」）；
// 指标在回执发出之后才累加，所以这里做有上限的短轮询，而不是假设顺序。
func doubleRunWaitRequest(t *testing.T, srv *testStack, method string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		for _, m := range metricsOf(t, srv) {
			if m.Name == "sump_requests_total" && m.Labels["method"] == method &&
				m.Labels["code"] == "ok" && m.Value >= 1 {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("聚合指标里没有 %s 的成功请求：experience 注入没有被调用", method)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// assertNoExperienceSection 断言 experience 为空：本测试栈没有写入经验，两个入口都不应
// 出现「经验线头」段。内容等价不在本用例范围内（R-026 §5）。
func assertNoExperienceSection(t *testing.T, legacy []protocol.Message, newResp []protocol.LLMMessage) {
	t.Helper()
	for _, m := range legacy {
		if strings.Contains(m.Content, "（经验线头）") {
			t.Fatalf("旧 context 的系统提示里不应出现经验段（experience 为空）: %q", m.Content)
		}
	}
	for _, m := range newResp {
		if strings.Contains(m.Content, "（经验线头）") {
			t.Fatalf("context_v2 的消息里不应出现经验段（context_v2 不投影 experience）: %q", m.Content)
		}
	}
}

// ---------- 主用例 ----------

// TestDoubleRunContextEquivalence 是双跑等价的主用例：
// 同一份库数据 → 旧 context（调用方托管链 + withTaskPrompt）与新 context_v2（memory 组装 +
// 调用方 system prepend）的最终模型链必须逐条一致，system 的条数 / 内容 / 位置也显式比较。
func TestDoubleRunContextEquivalence(t *testing.T) {
	fx := newDoubleRunFixture(t)

	// 双跑：两端读的是同一份库数据；旧入口额外需要调用方托管的 system prompt 与消息链。
	legacy := fx.callLegacy(doubleRunCallerSystem, 1)
	newResp := fx.mustContextV2(1)

	// experience 段：旧入口确实发起了 inject（指标可见），且两边都没有经验内容。
	doubleRunWaitRequest(t, fx.srv, "inject")
	assertNoExperienceSection(t, legacy.Messages, newResp.Messages)

	t.Run("system纳入的最终模型链", func(t *testing.T) {
		want := doubleRunWantProjection(fx)
		wantPrompt := doubleRunTaskPrompt(fx.taskName, fx.taskID)

		// 来源先钉住：旧 context 回执只有调用方 1 条 system；context_v2 只有 memory 生成的
		// 1 条任务提示。这样最终链的两条 system 各有明确出处，而不是从响应反推。
		assertSystemLayout(t, "旧 context 回执（withTaskPrompt 前）",
			doubleRunMessagesFromProtocol(legacy.Messages), []string{doubleRunCallerSystem})
		assertSystemLayout(t, "context_v2 回执",
			doubleRunMessagesFromLLM(newResp.Messages), []string{wantPrompt})
		for _, sys := range doubleRunSystemContents(doubleRunMessagesFromProtocol(legacy.Messages)) {
			if strings.Contains(sys, doubleRunMemoryPrefix) {
				t.Fatalf("本用例的调用方链完整覆盖库内历史，不应出现【记忆】块: %q", sys)
			}
		}

		legacyProj := fx.projectLegacyFinal(legacy)
		newProj := projectV2Final(newResp)

		// system：条数 / 内容 / 位置（两条，按序在链首）。
		wantSystems := []string{doubleRunCallerSystem, wantPrompt}
		assertSystemLayout(t, "旧最终模型链", legacyProj.Chain, wantSystems)
		assertSystemLayout(t, "新最终模型链", newProj.Chain, wantSystems)

		// 非 system 链逐字相等，且两端都等于独立写下的期望投影（防止“两边一起错”）。
		legacyCore := dropSystemMessages(legacyProj.Chain)
		newCore := dropSystemMessages(newProj.Chain)
		if !reflect.DeepEqual(legacyCore, newCore) {
			t.Fatalf("新旧非 system 链不一致:\n old = %s\n new = %s",
				doubleRunJSON(t, legacyCore), doubleRunJSON(t, newCore))
		}
		assertProjectionEqual(t, "旧 context（+withTaskPrompt）", legacyProj, want)
		assertProjectionEqual(t, "新 context_v2（+调用方 system）", newProj, want)

		// 顺序与 tool 配对（§10：另行验证消息顺序、tool 配对）。
		assertOrderAndToolPairing(t, "旧 context", legacyCore)
		assertOrderAndToolPairing(t, "新 context_v2", newCore)

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
//  1. 旧 raw 回执：system 恰好两条 —— 调用方 prompt + 【记忆】块（后者紧跟第一条 system）；
//  2. 新 raw 回执：system 恰好一条（任务提示），不含【记忆】块，也不折叠历史；
//  3. 非 system 链：旧链 == 新链的严格尾部（逐条、同序），差集长度 = 被折叠条数；
//  4. 被折叠掉的那一段（新链前缀）必须逐条出现在【记忆】块文本里（是折叠，不是丢弃）；
//  5. 最终模型链：旧 = [调用方, taskPrompt, 【记忆】, 尾部]，新 = [调用方, taskPrompt, 前缀, 尾部]。
//     去掉旧多出的那条 system 与新链被折叠的前缀后，两条链逐条相等——差异点恰好只有
//     「system 折叠块」与「被折叠的前缀」两类。
//  6. task / work / degraded / warnings 仍与主用例期望一致（差异只发生在 messages 的
//     system 形态与历史折叠，不影响其它 A 核心字段）。
func TestDoubleRunMemoryBlockDifference(t *testing.T) {
	fx := newDoubleRunFixture(t)

	// 调用方只托管 seq>=3 的尾部：seq1/seq2 在库内但不在链上 → 旧入口折叠进【记忆】块。
	partial := fx.callLegacy(doubleRunCallerSystem, 3)
	full := fx.mustContextV2(1)

	doubleRunWaitRequest(t, fx.srv, "inject")
	assertNoExperienceSection(t, partial.Messages, full.Messages)

	legacyRaw := doubleRunMessagesFromProtocol(partial.Messages)
	newRaw := doubleRunMessagesFromLLM(full.Messages)
	wantPrompt := doubleRunTaskPrompt(fx.taskName, fx.taskID)

	// 1) 旧 raw：调用方 system + 【记忆】块（位置紧随其后）。
	legacySystems := doubleRunSystemContents(legacyRaw)
	if len(legacySystems) != 2 || legacySystems[0] != doubleRunCallerSystem {
		t.Fatalf("旧 context raw 应产生「调用方 system + 【记忆】块」两条 system，实际: %+v", legacySystems)
	}
	memoryBlock := legacySystems[1]
	if !strings.HasPrefix(memoryBlock, doubleRunMemoryPrefix) {
		t.Fatalf("第二条 system 应是【记忆】块，实际: %q", memoryBlock)
	}
	if idx := doubleRunSystemIndexes(legacyRaw); !reflect.DeepEqual(idx, []int{0, 1}) {
		t.Fatalf("【记忆】块应紧跟第一条 system，实际 system 下标 = %v", idx)
	}

	// 2) 新 raw：只有 memory 生成的一条任务提示，不折叠、不注入【记忆】。
	assertSystemLayout(t, "context_v2 raw", newRaw, []string{wantPrompt})
	if strings.Contains(wantPrompt, doubleRunMemoryPrefix) {
		t.Fatalf("context_v2 不应注入【记忆】块: %q", wantPrompt)
	}

	// 3) 非 system 链：旧链 == 新链严格尾部，差集即被折叠的历史。
	legacyCore := dropSystemMessages(legacyRaw)
	newCore := dropSystemMessages(newRaw)
	if len(legacyCore) > len(newCore) {
		t.Fatalf("旧链的非 system 消息比新链还多: old=%d new=%d", len(legacyCore), len(newCore))
	}
	folded := len(newCore) - len(legacyCore)
	if folded == 0 {
		t.Fatalf("本用例应至少折叠 1 条历史，实际 folded=0（调用方链覆盖了全部历史？）")
	}
	if !reflect.DeepEqual(legacyCore, newCore[folded:]) {
		t.Fatalf("除【记忆】块外，新旧非 system 消息不一致:\n old = %s\n new[%d:] = %s",
			doubleRunJSON(t, legacyCore), folded, doubleRunJSON(t, newCore[folded:]))
	}
	// 旧尾部也必须等于独立期望投影的尾部（防止两端一起错）。
	wantCore := dropSystemMessages(doubleRunWantProjection(fx).Chain)
	if !reflect.DeepEqual(legacyCore, wantCore[folded:]) {
		t.Fatalf("旧链尾部与期望投影不一致:\n got = %s\nwant = %s",
			doubleRunJSON(t, legacyCore), doubleRunJSON(t, wantCore[folded:]))
	}
	// 被折叠的新前缀必须逐条出现在【记忆】块里（折叠 ≠ 丢弃）。
	for _, m := range newCore[:folded] {
		if strings.TrimSpace(m.Content) == "" {
			continue
		}
		if !strings.Contains(memoryBlock, m.Content) {
			t.Fatalf("被折叠的消息没有出现在【记忆】块里（内容丢失）: role=%s content=%q\nblock = %q",
				m.Role, m.Content, memoryBlock)
		}
	}

	// 5) 最终模型链的显式差异分类。
	legacyChain := fx.legacyModelChain(partial)
	newChain := doubleRunNewModelChain(full)
	legacyIdx := doubleRunSystemIndexes(legacyChain)
	newIdx := doubleRunSystemIndexes(newChain)
	if len(legacyIdx) != len(newIdx)+1 {
		t.Fatalf("旧最终链应恰好多出 1 条 system（【记忆】块）: old=%v new=%v",
			legacyIdx, newIdx)
	}
	if !reflect.DeepEqual(newIdx, []int{0, 1}) || len(legacyIdx) != 3 || legacyIdx[2] != 2 {
		t.Fatalf("最终链 system 位置不符: 旧=%v（期望 [0 1 2]），新=%v（期望 [0 1]）", legacyIdx, newIdx)
	}
	if legacyChain[2].Content != memoryBlock {
		t.Fatalf("旧最终链的多余 system 应是同一份【记忆】块:\n got = %q\nwant = %q",
			legacyChain[2].Content, memoryBlock)
	}
	if !reflect.DeepEqual(legacyChain[:2], newChain[:2]) {
		t.Fatalf("两条链的前缀 system 应逐条一致:\n old = %s\n new = %s",
			doubleRunJSON(t, legacyChain[:2]), doubleRunJSON(t, newChain[:2]))
	}
	// 差异恰好落在两类点上：去掉旧多出的【记忆】system、去掉新链被折叠的前缀，剩余逐条相等。
	oldRest := append(append([]normMessage{}, legacyChain[:2]...), legacyChain[3:]...)
	newRest := append(append([]normMessage{}, newChain[:2]...), newChain[2+folded:]...)
	if !reflect.DeepEqual(oldRest, newRest) {
		t.Fatalf("差异不止【记忆】块与折叠前缀:\n old去掉折叠块 = %s\n new去掉折叠前缀 = %s",
			doubleRunJSON(t, oldRest), doubleRunJSON(t, newRest))
	}
	// 非 system 尾部一致（同一事实的独立断言）。
	if !reflect.DeepEqual(dropSystemMessages(legacyChain), dropSystemMessages(newChain)[folded:]) {
		t.Fatalf("非 system 尾部不一致:\n old = %s\n new[%d:] = %s",
			doubleRunJSON(t, dropSystemMessages(legacyChain)), folded,
			doubleRunJSON(t, dropSystemMessages(newChain)[folded:]))
	}

	// 6) 差异只发生在 messages 的 system 形态 / 历史折叠上：task / work 仍一致。
	want := doubleRunWantProjection(fx)
	legacyProj := fx.projectLegacyFinal(partial)
	newProj := projectV2Final(full)
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
