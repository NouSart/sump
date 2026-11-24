package session

// context_v2 / trigger_complete 的包内测试（S-001 §3.1、§3.1.1）。
//
// 直接构造 Server + 真 SQLite（不启 WS）：这两个入口的全部业务语义都在共享实现里，
// 原生 handler 只做「解码 body → 调用 → 映射错误码」，所以这里测共享实现与 method 声明。
//
// 与源 af6cfc8:src/memory/server/context_v2_test.go 的差异：
//   - 授权（R-023 P4）在共享实现里已接；本文件的辅助统一以「载荷 work 的 agent」调用，
//     跨 work 拒绝用例在 authorize_entries_test.go 与 testing/tests 红队；
//   - work_id 为空字符串的旧记录不再兜底（R-014 T4），相关断言改为「不得出现」；
//   - image_index 未搬（D7），不再断言图片行。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// newContextV2TestServer 直接构造 memory Server（只有库依赖）并建一个 work / task。
func newContextV2TestServer(t *testing.T, workID string) (*Server, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}

	ctx := context.Background()
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID: workID, Root: workID, Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Goal: "上下文测试", Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatalf("登记 work 失败: %v", err)
	}
	task, err := st.OpenTask("conv-"+workID, "上下文任务")
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if _, err := st.DB().Exec(`UPDATE works SET task = ? WHERE id = ?`, task.ID, workID); err != nil {
		t.Fatalf("给 work 挂任务失败: %v", err)
	}
	return s, st, task.ID
}

// insertContextV2Message 直接写一条消息（work_id / visibility / meta 都是测试要控制的新列）。
func insertContextV2Message(t *testing.T, st *store.Store, workID, taskID, role, kind, content, meta string, seq int64) string {
	t.Helper()
	res, err := st.DB().Exec(
		`INSERT INTO messages(conversation_id, role, content, kind, meta, seq, task_id, work_id, visibility, created_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, 'work', ?)`,
		"conv-"+workID, role, content, kind, meta, seq, taskID, workID, time.Now().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatalf("写入消息失败: %v", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return strconv.FormatInt(id, 10)
}

// callContextV2 调用共享实现；帧头 work 与身份按「本 work 的 agent」构造
// （与生产链路一致：入口把原帧 from 透传给 session）。
func callContextV2(t *testing.T, s *Server, req protocol.ContextRequest) (protocol.ContextResponse, error) {
	t.Helper()
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.contextV2(context.Background(), body,
		caller{work: req.Work, from: spv2.AgentRef("reasoner")})
	if err != nil {
		return protocol.ContextResponse{}, err
	}
	resp, ok := out.(protocol.ContextResponse)
	if !ok {
		t.Fatalf("context_v2 应返回 protocol.ContextResponse，实际 %T", out)
	}
	return resp, nil
}

// callTriggerComplete 调用共享实现。
func callTriggerComplete(t *testing.T, s *Server, req protocol.TriggerCompletePayload) (protocol.TriggerCompleteResult, error) {
	t.Helper()
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.triggerComplete(context.Background(), body,
		caller{work: req.Work, from: spv2.AgentRef("reasoner")})
	if err != nil {
		return protocol.TriggerCompleteResult{}, err
	}
	res, ok := out.(protocol.TriggerCompleteResult)
	if !ok {
		t.Fatalf("trigger_complete 应返回 protocol.TriggerCompleteResult，实际 %T", out)
	}
	return res, nil
}

// TestContextV2FirstAssembleSnapshotReuseAndComplete 覆盖四分支中的前三支：
// 首次 pending → 生成；assembled → 同一 snapshot_id；completed → 只补 ACK（不带消息链）。
func TestContextV2FirstAssembleSnapshotReuseAndComplete(t *testing.T) {
	const workID = "w-context"
	s, st, taskID := newContextV2TestServer(t, workID)

	// 故意乱序写入：组装必须按 seq 排，而不是按落库顺序。
	trigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "问题一", "", 2)
	insertContextV2Message(t, st, workID, taskID, "assistant", "assistant", "答复一", "", 3)
	insertContextV2Message(t, st, workID, taskID, "assistant", "tool_call", "pwd",
		`{"tool":"shell__run","call_id":"call-1","arguments":"{\"command\":\"pwd\"}"}`, 4)
	insertContextV2Message(t, st, workID, taskID, "tool", "tool_result", "工作目录",
		`{"call_id":"call-1"}`, 5)
	insertContextV2Message(t, st, workID, taskID, "user", "user", "最早的一条", "", 1)
	// 旧记录：work_id 为空——不再兜底（R-014 T4），不得出现在组装结果里。
	insertContextV2Message(t, st, "", taskID, "user", "user", "旧任务消息", "", 6)

	req := protocol.ContextRequest{
		Work:    workID,
		Trigger: protocol.Trigger{Kind: "user", Item: trigger},
		Schema:  contextV2Schema,
	}
	resp, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("首次组装失败: %v", err)
	}
	if resp.Schema != contextV2Schema || resp.SnapshotID == "" || resp.AssembledAt == "" {
		t.Fatalf("响应元信息不全: %+v", resp)
	}
	if resp.Task.Name != "上下文任务" || resp.Task.ID != taskID {
		t.Fatalf("任务投影不对: %+v", resp.Task)
	}
	if resp.Work.ID != workID || resp.Work.Task != taskID || resp.Work.Owner != "owner" || resp.Work.Status != spv2.WorkRunning {
		t.Fatalf("work 投影不对: %+v", resp.Work)
	}
	if expires, err := time.Parse(time.RFC3339Nano, resp.ExpiresAt); err != nil || !expires.After(time.Now()) {
		t.Fatalf("expires_at 应在将来: %q err=%v", resp.ExpiresAt, err)
	}

	// 1) system 必须是第一条。
	if len(resp.Messages) == 0 || resp.Messages[0].Role != "system" {
		t.Fatalf("第一条消息应是 system: %+v", resp.Messages)
	}
	// 2) 历史按 seq：最早的一条 → 问题一 → 答复一；work_id 为空的旧记录不进来（T4）。
	var order []string
	for _, m := range resp.Messages {
		switch m.Content {
		case "最早的一条", "问题一", "答复一", "旧任务消息":
			order = append(order, m.Content)
		}
	}
	wantOrder := []string{"最早的一条", "问题一", "答复一"}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("历史顺序不对: %v，期望 %v（messages=%+v）", order, wantOrder, resp.Messages)
	}
	// 3) 本轮 Images 传空（D7 未接 image_index）：不该有多出来的图片行。
	if hasMessage(resp.Messages, "user", "[图片") {
		t.Fatalf("P2 不接 image_index，不应渲染图片行: %+v", resp.Messages)
	}
	// 4) tool 配对：assistant.tool_calls 后紧跟 role=tool，call_id 一致、参数原样带上。
	toolCallIdx := -1
	for i, m := range resp.Messages {
		if m.Role == "assistant" && len(m.ToolCalls) == 1 && m.ToolCalls[0].ID == "call-1" {
			toolCallIdx = i
			if m.ToolCalls[0].Name != "shell__run" || !strings.Contains(m.ToolCalls[0].Arguments, "pwd") {
				t.Fatalf("工具调用字段不对: %+v", m.ToolCalls[0])
			}
		}
	}
	if toolCallIdx < 0 || toolCallIdx+1 >= len(resp.Messages) {
		t.Fatalf("没有找到工具调用配对: %+v", resp.Messages)
	}
	pair := resp.Messages[toolCallIdx+1]
	if pair.Role != "tool" || pair.ToolCallID != "call-1" || pair.Content != "工作目录" {
		t.Fatalf("工具结果配对不对: %+v", pair)
	}

	// 快照落库、触发状态 assembled（响应上限之外的可核对事实）。
	if state, snapshotID, err := st.GetTriggerState(context.Background(), workID, "user", trigger); err != nil ||
		state != store.TriggerStateAssembled || snapshotID != resp.SnapshotID {
		t.Fatalf("触发状态不对: state=%q snapshot=%q err=%v", state, snapshotID, err)
	}

	// assembled 分支：库里新增消息也不重新组装，snapshot_id 不变、消息链逐字段相同。
	insertContextV2Message(t, st, workID, taskID, "user", "user", "后来才有的消息", "", 50)
	second, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("assembled 重复触发失败: %v", err)
	}
	if second.SnapshotID != resp.SnapshotID {
		t.Fatalf("重复触发不应换快照: %q vs %q", second.SnapshotID, resp.SnapshotID)
	}
	if !reflect.DeepEqual(second.Messages, resp.Messages) {
		t.Fatalf("assembled 分支不应重新组装:\nold=%+v\nnew=%+v", resp.Messages, second.Messages)
	}

	// completed 分支：trigger_complete 之后只补 ACK——不再返回消息链，避免把已收口的一轮重推理。
	if _, err := callTriggerComplete(t, s, protocol.TriggerCompletePayload{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: trigger},
	}); err != nil {
		t.Fatalf("trigger_complete 失败: %v", err)
	}
	insertContextV2Message(t, st, workID, taskID, "assistant", "assistant", "完成后的新答复", "", 60)
	third, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("completed 后取 context 失败: %v", err)
	}
	if !third.Completed || third.TriggerState != store.TriggerStateCompleted {
		t.Fatalf("completed 分支必须回 completed=true / trigger_state=completed: %+v", third)
	}
	if third.SnapshotID != resp.SnapshotID {
		t.Fatalf("completed 的 ACK 应指向那份已完成快照: %q vs %q", third.SnapshotID, resp.SnapshotID)
	}
	if len(third.Messages) != 0 {
		t.Fatalf("completed 分支不得再返回消息链: %+v", third.Messages)
	}
	if len(third.Warnings) == 0 {
		t.Fatalf("completed 分支要显式说明只补 ACK: %+v", third)
	}
}

// hasMessage 判断消息链里有没有某 role 且内容包含 substr 的消息。
func hasMessage(msgs []protocol.LLMMessage, role, substr string) bool {
	for _, m := range msgs {
		if m.Role == role && strings.Contains(m.Content, substr) {
			return true
		}
	}
	return false
}

// TestContextV2ExpiredSnapshotNotFound 四分支的第四支：assembled 且快照过期 → not_found，
// 而且不得静默重建；completed 是例外——完成事实不随快照消失，只补 ACK。
func TestContextV2ExpiredSnapshotNotFound(t *testing.T) {
	const workID = "w-expired"
	s, st, taskID := newContextV2TestServer(t, workID)
	item := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	req := protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item}}

	first, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("首次组装失败: %v", err)
	}
	expireSnapshot(t, st, workID, "user", item)

	if _, err := callContextV2(t, s, req); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("assembled 且快照过期应 not_found，实际: %v", err)
	}

	// 不得静默重建：快照行数不变，触发状态也不被改成 pending。
	if n := countSnapshots(t, st); n != 1 {
		t.Fatalf("过期后不应重建快照，实际快照数 %d", n)
	}
	if state, _, err := st.GetTriggerState(context.Background(), workID, "user", item); err != nil || state != store.TriggerStateAssembled {
		t.Fatalf("过期后触发状态不应被改写: state=%q err=%v", state, err)
	}

	// completed 之后即使快照过期，ACK 依然成立：「已完成」这个事实不会因为快照被清理而退化，
	// 但也不能因此重新组装（快照数保持 1）。
	if _, err := callTriggerComplete(t, s, protocol.TriggerCompletePayload{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item},
	}); err != nil {
		t.Fatalf("trigger_complete 失败: %v", err)
	}
	ack, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("completed 且快照过期时 ACK 应仍成立，实际: %v", err)
	}
	if !ack.Completed || len(ack.Messages) != 0 {
		t.Fatalf("过期后的 completed 应答应只有 ACK: %+v", ack)
	}
	if n := countSnapshots(t, st); n != 1 {
		t.Fatalf("completed+过期后不应重建快照，实际快照数 %d（首次 snapshot=%s）", n, first.SnapshotID)
	}
}

// TestContextV2CompletedAckSurvivesSnapshotDelete 完成记录与快照解耦：
// 快照行被清理后，同一触发仍只补 ACK，且不得借此重新组装出第二份投影。
func TestContextV2CompletedAckSurvivesSnapshotDelete(t *testing.T) {
	const workID = "w-completed-ack"
	s, st, taskID := newContextV2TestServer(t, workID)
	item := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	req := protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item}}

	first, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("首次组装失败: %v", err)
	}
	if _, err := callTriggerComplete(t, s, protocol.TriggerCompletePayload{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item},
	}); err != nil {
		t.Fatalf("trigger_complete 失败: %v", err)
	}
	// 模拟保留期后的清理：整表快照删掉。
	if _, err := st.DB().Exec(`DELETE FROM context_snapshots`); err != nil {
		t.Fatal(err)
	}
	// task 行也删掉：任何「重新组装」的尝试都会在这里失败，只补 ACK 不该碰 tasks。
	deleteTaskRow(t, st, taskID, workID)

	ack, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("快照与 task 都被清掉后，completed 仍应只补 ACK: %v", err)
	}
	if !ack.Completed || ack.TriggerState != store.TriggerStateCompleted || len(ack.Messages) != 0 {
		t.Fatalf("ACK 形状不对: %+v", ack)
	}
	if ack.SnapshotID != first.SnapshotID {
		t.Fatalf("ACK 应指回原快照 id: %q vs %q", ack.SnapshotID, first.SnapshotID)
	}
	if n := countSnapshots(t, st); n != 0 {
		t.Fatalf("completed 分支不得重建快照，实际 %d", n)
	}
}

// expireSnapshot 把某快照的 expires_at 改到过去（模拟 24h 保留期到期）。
func expireSnapshot(t *testing.T, st *store.Store, work, kind, item string) {
	t.Helper()
	past := time.Now().Add(-time.Hour).UTC().Format(time.RFC3339Nano)
	res, err := st.DB().Exec(
		`UPDATE context_snapshots SET expires_at = ? WHERE work = ? AND trigger_kind = ? AND trigger_item = ?`,
		past, work, kind, item)
	if err != nil {
		t.Fatalf("改快照过期时间失败: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("应改到 1 行快照，实际 %d", n)
	}
}

// countSnapshots 统计快照总数（验证「不得静默重建」）。
func countSnapshots(t *testing.T, st *store.Store) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM context_snapshots`).Scan(&n); err != nil {
		t.Fatalf("统计快照失败: %v", err)
	}
	return n
}

// TestContextV2AssembledSnapshotDoesNotReassemble assembled 分支只读快照、不重新组装。
//
// 构造办法：首次组装后删掉 work 所属的 tasks 行。重新组装必须先过 TaskIDOfWork + TaskByID，
// task 行没了必然失败；而只读快照根本不碰 tasks 表。于是「删掉 task 仍拿回原快照与消息链」
// 就把「assembled 分支没有重新组装」变成可观测事实——把 assembled 改成重新组装的变异会翻脸。
func TestContextV2AssembledSnapshotDoesNotReassemble(t *testing.T) {
	const workID = "w-assembled-snapshot"
	s, st, taskID := newContextV2TestServer(t, workID)
	item := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	req := protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item}}

	first, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("首次组装失败: %v", err)
	}
	deleteTaskRow(t, st, taskID, workID)

	got, err := callContextV2(t, s, req)
	if err != nil {
		t.Fatalf("assembled 分支不应重新组装（重新组装会因 task 行已删而失败）: %v", err)
	}
	if got.SnapshotID != first.SnapshotID || !reflect.DeepEqual(got.Messages, first.Messages) {
		t.Fatalf("assembled 分支应原样返回首次快照:\nsnapshot=%q vs %q\nmessages=%+v",
			got.SnapshotID, first.SnapshotID, got.Messages)
	}
	if n := countSnapshots(t, st); n != 1 {
		t.Fatalf("只读快照不应产生第二份快照，实际 %d", n)
	}
}

// TestContextV2CompletedIsPerTrigger 已完成只针对这一份触发：
// 换一个 trigger.item 就是新一轮，照常组装，不会被上一轮的 ACK 卡住。
func TestContextV2CompletedIsPerTrigger(t *testing.T) {
	const workID = "w-completed-next"
	s, st, taskID := newContextV2TestServer(t, workID)
	first := insertContextV2Message(t, st, workID, taskID, "user", "user", "上一轮", "", 1)
	next := insertContextV2Message(t, st, workID, taskID, "user", "user", "新一轮", "", 2)

	if _, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: first}}); err != nil {
		t.Fatalf("首轮组装失败: %v", err)
	}
	if _, err := callTriggerComplete(t, s, protocol.TriggerCompletePayload{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: first}}); err != nil {
		t.Fatalf("首轮完成失败: %v", err)
	}
	// 同一触发：ACK；换 trigger.item：新一轮正常组装。
	ack, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: first}})
	if err != nil || !ack.Completed || len(ack.Messages) != 0 {
		t.Fatalf("同一触发应只补 ACK: %+v err=%v", ack, err)
	}
	fresh, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: next}})
	if err != nil {
		t.Fatalf("换 trigger.item 应正常组装: %v", err)
	}
	if fresh.Completed || len(fresh.Messages) == 0 || fresh.SnapshotID == ack.SnapshotID {
		t.Fatalf("新一轮应是新快照与完整消息链: %+v", fresh)
	}
	if n := countSnapshots(t, st); n != 2 {
		t.Fatalf("两轮各一份快照，实际 %d", n)
	}
}

// TestContextV2RejectsForeignTriggerItem 触发条目必须属于载荷里的 work：
// 别的 work 的消息不能拿来当本 work 的触发。这是 P2 唯一的读边界（按 work 归属），
// 授权三重（from / owner / 身份）留到 P4。
func TestContextV2RejectsForeignTriggerItem(t *testing.T) {
	s, st, taskID := newContextV2TestServer(t, "w-owner")
	own := insertContextV2Message(t, st, "w-owner", taskID, "user", "user", "本 work 的触发", "", 1)
	foreign := insertContextV2Message(t, st, "w-other", taskID, "user", "user", "别的 work 的触发", "", 2)
	ctx := context.Background()

	for _, tc := range []struct{ name, item string }{
		{"别的 work 的消息", foreign},
		{"不存在的消息", "999999"},
		{"不是十进制", "not-an-id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := callContextV2(t, s, protocol.ContextRequest{
				Work: "w-owner", Trigger: protocol.Trigger{Kind: "user", Item: tc.item},
			}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
				t.Fatalf("trigger.item 不属于本 work 应 invalid_argument，实际: %v", err)
			}
			if _, err := callTriggerComplete(t, s, protocol.TriggerCompletePayload{
				Work: "w-owner", Trigger: protocol.Trigger{Kind: "user", Item: tc.item},
			}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
				t.Fatalf("trigger_complete 对不属于本 work 的 item 应 invalid_argument，实际: %v", err)
			}
		})
	}
	// 拒绝不得留下副作用：受害者这一轮仍没有快照与触发状态。
	if n := countSnapshots(t, st); n != 0 {
		t.Fatalf("被拒绝的请求不应产生快照，实际 %d", n)
	}
	if state, _, err := st.GetTriggerState(ctx, "w-owner", "user", foreign); err != nil || state != "" {
		t.Fatalf("被拒绝的请求不得改写触发状态: state=%q err=%v", state, err)
	}
	// 本 work 的触发照常组装（拒绝的是别人的条目，不是这个 work 本身）。
	if got, err := callContextV2(t, s, protocol.ContextRequest{
		Work: "w-owner", Trigger: protocol.Trigger{Kind: "user", Item: own},
	}); err != nil || len(got.Messages) == 0 {
		t.Fatalf("本 work 的触发应正常组装: %+v err=%v", got, err)
	}
}

// deleteTaskRow 删掉任务行，并确认「重新组装」这条路确实走不通（TaskByID 必失败）。
// 只读快照的路径不碰 tasks 表，所以这一步不影响它；若哪天重新组装不再需要 tasks，
// 这里会先报「测试构造失效」，避免用例悄悄变成永真。
func deleteTaskRow(t *testing.T, st *store.Store, taskID, workID string) {
	t.Helper()
	res, err := st.DB().Exec(`DELETE FROM tasks WHERE id = ?`, taskID)
	if err != nil {
		t.Fatalf("删除任务行失败: %v", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		t.Fatalf("应删除 1 行任务（id=%s work=%s），实际 %d", taskID, workID, n)
	}
	if _, err := st.TaskByID(taskID); err == nil {
		t.Fatalf("测试构造失效：tasks 行已删除（id=%s），TaskByID 仍成功", taskID)
	}
}

// TestContextV2RejectsBadTrigger 参数与归属错误：item 不属于 work / 参数缺失 → invalid_argument；
// work 不存在 → not_found；work 没有任务 → not_found。
func TestContextV2RejectsBadTrigger(t *testing.T) {
	const workID = "w-bad"
	s, st, taskID := newContextV2TestServer(t, workID)
	item := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	// 两个 not_found 分支的构造：item 必须属于载荷里的 work（否则先撞归属检查）。
	ghostItem := insertContextV2Message(t, st, "w-ghost", taskID, "user", "user", "幽灵 work 的触发", "", 2)
	noTaskItem := insertContextV2Message(t, st, "w-no-task", taskID, "user", "user", "没任务 work 的触发", "", 3)
	ctx := context.Background()
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID: "w-no-task", Root: "w-no-task", Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		req  protocol.ContextRequest
		code string
	}{
		{
			name: "item 不存在",
			req:  protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: "999999"}},
			code: spv2.CodeInvalidArgument,
		},
		{
			name: "item 不是十进制",
			req:  protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: "abc"}},
			code: spv2.CodeInvalidArgument,
		},
		{
			name: "work 为空",
			req:  protocol.ContextRequest{Work: "  ", Trigger: protocol.Trigger{Kind: "user", Item: item}},
			code: spv2.CodeInvalidArgument,
		},
		{
			name: "trigger.kind 为空",
			req:  protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "", Item: item}},
			code: spv2.CodeInvalidArgument,
		},
		{
			name: "trigger.item 为空",
			req:  protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: ""}},
			code: spv2.CodeInvalidArgument,
		},
		{
			name: "不支持的 schema",
			req:  protocol.ContextRequest{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item}, Schema: "context/99"},
			code: spv2.CodeInvalidArgument,
		},
		{
			name: "work 不存在",
			req:  protocol.ContextRequest{Work: "w-ghost", Trigger: protocol.Trigger{Kind: "user", Item: ghostItem}},
			code: spv2.CodeNotFound,
		},
		{
			name: "work 存在但没有任务",
			req:  protocol.ContextRequest{Work: "w-no-task", Trigger: protocol.Trigger{Kind: "user", Item: noTaskItem}},
			code: spv2.CodeNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := callContextV2(t, s, tc.req); !spv2.IsCode(err, tc.code) {
				t.Fatalf("应返回 %s，实际: %v", tc.code, err)
			}
		})
	}
}

// TestTriggerCompletePendingConflict pending / 未组装 → conflict；assembled → 成功且幂等。
func TestTriggerCompletePendingConflict(t *testing.T) {
	const workID = "w-complete"
	s, st, taskID := newContextV2TestServer(t, workID)
	item := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	payload := protocol.TriggerCompletePayload{Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item}}

	// 还没调过 context_v2（pending / 没有状态记录）→ conflict。
	if _, err := callTriggerComplete(t, s, payload); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("pending 触发调 trigger_complete 应 conflict，实际: %v", err)
	}
	// 触发条目不存在 → invalid_argument（先于状态判定）。
	if _, err := callTriggerComplete(t, s, protocol.TriggerCompletePayload{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: "999999"},
	}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("不存在的 trigger.item 应 invalid_argument，实际: %v", err)
	}
	// 组装后再完成：成功；重复调用幂等成功。
	if _, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: item},
	}); err != nil {
		t.Fatalf("组装失败: %v", err)
	}
	res, err := callTriggerComplete(t, s, payload)
	if err != nil || !res.Completed {
		t.Fatalf("assembled 后 trigger_complete 应成功: %+v err=%v", res, err)
	}
	if res, err := callTriggerComplete(t, s, payload); err != nil || !res.Completed {
		t.Fatalf("重复 trigger_complete 应幂等成功: %+v err=%v", res, err)
	}
}

// TestContextV2MethodDeclarations 注册声明：方法名、非工具、schema 是 object 且必填项齐全。
func TestContextV2MethodDeclarations(t *testing.T) {
	for _, m := range []spv2.Method{contextV2Method(), triggerCompleteMethod()} {
		if m.Tool {
			t.Errorf("%s 不应暴露给模型（Tool 必须为 false）", m.Name)
		}
		if m.Input == nil || m.Input["type"] != "object" || m.Output["type"] != "object" {
			t.Errorf("%s 的 input/output 必须是 object schema: %+v / %+v", m.Name, m.Input, m.Output)
		}
		if !schemaRequires(m.Input, "work") || !schemaRequires(m.Input, "trigger") {
			t.Errorf("%s 的 input 应必填 work 与 trigger: %+v", m.Name, m.Input)
		}
	}
	if contextV2Method().Name != "context_v2" || triggerCompleteMethod().Name != "trigger_complete" {
		t.Fatal("method 名不符合契约（context_v2 / trigger_complete）")
	}
	// 输出契约：schema 与 trigger_state 必填（调用方据此判断这是新一轮投影还是只补 ACK）；
	// messages 在有消息的分支里存在，但 completed 分支按 §3.1.1 只补 ACK、不带消息，
	// 所以 messages 出现在 properties 里而不在 required 里。
	if !schemaRequires(contextV2Method().Output, "snapshot_id") {
		t.Fatalf("context_v2 的 output 应必填 snapshot_id: %+v", contextV2Method().Output)
	}
	if !schemaHasProperty(contextV2Method().Output, "messages") ||
		!schemaHasProperty(contextV2Method().Output, "task") ||
		!schemaHasProperty(contextV2Method().Output, "work") ||
		!schemaHasProperty(contextV2Method().Output, "degraded") ||
		!schemaHasProperty(contextV2Method().Output, "warnings") ||
		!schemaHasProperty(contextV2Method().Output, "completed") ||
		!schemaHasProperty(contextV2Method().Output, "trigger_state") {
		t.Fatalf("context_v2 的 output 缺少 A 切片核心字段: %+v", contextV2Method().Output)
	}
	if !schemaRequires(triggerCompleteMethod().Output, "completed") {
		t.Fatalf("trigger_complete 的 output 应必填 completed: %+v", triggerCompleteMethod().Output)
	}

	// 注册本身可调用（重复注册是覆盖，不 panic）。这里用一个未启动的 service 骨架。
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s := &Server{
		Service: service.New(service.Config{Name: "memory-test", Logger: log.New(io.Discard, "", 0)}),
		cfg:     Config{Store: st},
		logger:  log.New(io.Discard, "", 0),
	}
	s.registerContextV2()
	s.registerContextV2()
}

// schemaRequires 判断 object schema 的 required 里有没有某字段。
func schemaRequires(schema map[string]any, field string) bool {
	required, ok := schema["required"].([]any)
	if !ok {
		return false
	}
	for _, v := range required {
		if v == field {
			return true
		}
	}
	return false
}

// schemaHasProperty 判断 object schema 的 properties 里声明了某字段。
func schemaHasProperty(schema map[string]any, field string) bool {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return false
	}
	_, ok = props[field]
	return ok
}
