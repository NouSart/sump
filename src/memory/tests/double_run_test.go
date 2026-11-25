package tests

// R-026 P2c 的 golden 回归（旧双跑测试的替代）：旧 `context` 实现已删除，本用例用
// **独立字面量**钉住现行 `context/2` 的基座投影：
//   - 任务提示 system + 按 seq 的历史 + 工具配对，逐条相等（S-001 §3.1 / §10）；
//   - task / work / degraded / warnings 与独立期望一致；
//   - experience 为空时门面不注入【记忆】块；
//   - 快照稳定性 / 过期 not_found 仍走真实门面 + session（随机 ID 与时间戳只做形态断言）。
//
// 不 import reasoner（架构红线：服务之间只共享协议）；taskPrompt 文字在测试里镜像，
// memory 侧任何文字漂移都会红灯。
//
// 离线、可重复：设置中心是 httptest 替身；消息写入与 context 调用都走真 WS + 真 SQLite
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
	doubleRunGoal     = "验证上下文基座投影"
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

// doubleRunRows 是本次 golden 回归用的消息链：user → assistant → user → tool_call → tool_result。
// tool 记录放在最后：assemble 的固定顺序是「历史 → 图片 → 工具配对」。
//
// Seq 是这条消息**应有的逻辑顺序**：v2 路径下 seq 由 memory 的 seq_counters 在事务内分配
// （S-001 §3.0 规则 2），所以本用例按 Seq 升序写入，让分配出来的 seq 与逻辑顺序一致。
// 「乱序写入也要按 seq 读」这件事由 store/seq_order_test.go 与 session/context_base_test.go
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

// ---------- 切换后的 golden 回归 ----------

// callContext 通过 WS 调 context/2（门面组合）；trigger.item 必须是已落库消息的
// 十进制 id。错误原样返回，供“过期 → not_found”用例判断错误码。
func (fx *doubleRunFixture) callContext(triggerSeq int64) (protocol.ContextResponse, error) {
	fx.t.Helper()
	item, ok := fx.itemIDs[triggerSeq]
	if !ok {
		fx.t.Fatalf("seq=%d 没有落库消息 id", triggerSeq)
	}
	if _, err := strconv.ParseInt(item, 10, 64); err != nil {
		fx.t.Fatalf("trigger.item %q 不是十进制字符串: %v", item, err)
	}
	req := protocol.ContextRequest{
		Work:    doubleRunWorkID,
		Trigger: protocol.Trigger{Kind: "user", Item: item},
		Schema:  "context/2",
	}
	body, err := doubleRunCall(fx.t, fx.srv.WsURL(), doubleRunWorkID, doubleRunAgent, "context", req)
	if err != nil {
		return protocol.ContextResponse{}, err
	}
	var resp protocol.ContextResponse
	if err := protocol.DecodeRaw(body, &resp); err != nil {
		fx.t.Fatalf("context 回执无法解码: %v", err)
	}
	return resp, nil
}

// mustContext 是 callContext 的失败即终止版本。
func (fx *doubleRunFixture) mustContext(triggerSeq int64) protocol.ContextResponse {
	fx.t.Helper()
	resp, err := fx.callContext(triggerSeq)
	if err != nil {
		fx.t.Fatalf("context（trigger seq=%d）失败: %v", triggerSeq, err)
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

// normToolCall 是 tool_calls 的规范化视图。
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

// normWork 是 work 的规范化视图（时间戳排除）。
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

// doubleRunTaskPrompt 是 memory/session/assemble/render.go 的任务提示文字镜像。
// 这里不 import memory/session/assemble：测试只认最终线上文字。
func doubleRunTaskPrompt(name, id string) string {
	return fmt.Sprintf("当前任务：《%s》（id=%s）。你的所有动作都属于这个任务；换任务时上下文会重新开始。", name, id)
}

// doubleRunMessagesFromLLM 把 context/2 的消息投影成规范化视图（保留 system）。
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

// doubleRunWantMessages 是独立写下的期望基座：任务提示 system + 按 seq 的历史 +
// 固定顺序的工具配对。它不从 context 回执反推，任何投影漂移都会红灯。
func doubleRunWantMessages(fx *doubleRunFixture) []normMessage {
	return []normMessage{
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
	}
}

// doubleRunWantTask / doubleRunWantWork 是 task / work 的独立期望。
func doubleRunWantTask(fx *doubleRunFixture) normTask {
	return normTask{
		ID: fx.taskID, ConversationID: doubleRunConvID,
		Name: doubleRunTaskName, Status: store.TaskActive,
	}
}

func doubleRunWantWork(fx *doubleRunFixture) normWork {
	return normWork{
		ID: doubleRunWorkID, Task: fx.taskID, Class: spv2.ClassRequest,
		Status: spv2.WorkRunning, Owner: doubleRunOwner,
		Agent: doubleRunAgent, Entry: doubleRunEntry, Goal: doubleRunGoal,
	}
}

// doubleRunJSON 是失败信息里的可读投影。
func doubleRunJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return string(raw)
}

// TestContextGoldenBaseProjection 是旧双跑测试的 golden 回归：
// 同一份库数据只走现行 context/2，基座投影必须逐条等于独立字面量；
// 快照稳定性与过期 not_found 仍走真实门面 + session。
func TestContextGoldenBaseProjection(t *testing.T) {
	fx := newDoubleRunFixture(t)
	resp := fx.mustContext(1)

	got := doubleRunMessagesFromLLM(resp.Messages)
	want := doubleRunWantMessages(fx)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("context 基座投影与 golden 不一致:\n got = %s\nwant = %s",
			doubleRunJSON(t, got), doubleRunJSON(t, want))
	}
	// experience 为空：门面不应注入【记忆】块，也不得出现经验段。
	for _, m := range resp.Messages {
		if strings.Contains(m.Content, "【记忆】") || strings.Contains(m.Content, "（经验线头）") {
			t.Fatalf("本栈没有经验，不应出现记忆块: %q", m.Content)
		}
	}

	gotTask := normTask{ID: resp.Task.ID, ConversationID: resp.Task.ConversationID, Name: resp.Task.Name, Status: resp.Task.Status}
	if !reflect.DeepEqual(gotTask, doubleRunWantTask(fx)) {
		t.Fatalf("task 投影不一致:\n got = %s\nwant = %s",
			doubleRunJSON(t, gotTask), doubleRunJSON(t, doubleRunWantTask(fx)))
	}
	gotWork := normWork{
		ID: resp.Work.ID, Task: resp.Work.Task, Class: resp.Work.Class, Status: resp.Work.Status,
		Owner: resp.Work.Owner, Agent: resp.Work.Agent, Entry: resp.Work.Entry, Goal: resp.Work.Goal,
	}
	if !reflect.DeepEqual(gotWork, doubleRunWantWork(fx)) {
		t.Fatalf("work 投影不一致:\n got = %s\nwant = %s",
			doubleRunJSON(t, gotWork), doubleRunJSON(t, doubleRunWantWork(fx)))
	}
	if resp.Degraded != nil {
		t.Fatalf("本次回归不应降级，实际 degraded = %+v", resp.Degraded)
	}
	if len(resp.Warnings) != 0 {
		t.Fatalf("本次回归不应产生 warning，实际 = %+v", resp.Warnings)
	}
	if resp.SnapshotID == "" || resp.AssembledAt == "" {
		t.Fatalf("context 应返回 snapshot_id / assembled_at: %+v", resp)
	}
	if expires, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt); err != nil || !expires.After(time.Now()) {
		t.Fatalf("expires_at 应在将来: %q err=%v", resp.ExpiresAt, err)
	}

	t.Run("快照稳定性", func(t *testing.T) {
		second := fx.mustContext(1)
		if second.SnapshotID != resp.SnapshotID {
			t.Fatalf("同一 trigger 第二次的 snapshot_id 变了: %q vs %q", second.SnapshotID, resp.SnapshotID)
		}
		firstJSON, err := json.Marshal(resp.Messages)
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

		// 快照生成后再往库里写一条消息：仍不得重新组装。
		fx.appendMessage(doubleRunRow{Seq: 6, Role: "user", Kind: "user", Content: "快照生成后才写入的消息"}, 6)
		third := fx.mustContext(1)
		if third.SnapshotID != resp.SnapshotID {
			t.Fatalf("库内新增消息后 snapshot_id 变了（被重新组装）: %q vs %q", third.SnapshotID, resp.SnapshotID)
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

		if _, err := fx.callContext(1); !spv2.IsCode(err, spv2.CodeNotFound) {
			t.Fatalf("快照过期后 context 应返回 not_found，实际: %v", err)
		}

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
		if state != store.TriggerStateAssembled || snapshotID != resp.SnapshotID {
			t.Fatalf("过期后触发状态不应被改写: state=%q snapshot=%q（期望 assembled / %q）",
				state, snapshotID, resp.SnapshotID)
		}
	})
}
