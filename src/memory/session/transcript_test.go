package session

// transcript 的包内测试（S-001 §3.2）。
//
// 契约（S-001 §3.2）只给了请求 / 响应形状与「与 context 共用读取代码」，没定义 budget
// 与文本格式；下列用例把本实现的默认口径钉成可执行事实（见 transcript.go 文件头）：
//   - snapshot_id 必填，且必须就是那份快照：不能退化成「这个 work 的最新快照」；
//   - 文本按消息顺序 `role: content`，tool 消息附 tool_call_id；
//   - budget.max_chars 按字符计，0 = 不额外限制；truncated = 末尾被截；
//     omitted = 因预算整条丢弃的条数（被截成前缀的那条不算）；
//   - 只读：不改触发状态、不产生新快照。

import (
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// callTranscript 调共享实现；帧头 work 与身份按「本 work 的 agent」构造（与生产链路一致）。
func callTranscript(t *testing.T, s *Server, req protocol.TranscriptPayload) (protocol.TranscriptResult, error) {
	t.Helper()
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.transcript(context.Background(), body,
		caller{work: req.Work, from: spv2.AgentRef("reasoner")})
	if err != nil {
		return protocol.TranscriptResult{}, err
	}
	res, ok := out.(protocol.TranscriptResult)
	if !ok {
		t.Fatalf("transcript 应返回 protocol.TranscriptResult，实际 %T", out)
	}
	return res, nil
}

// newTranscriptWork 再登记一个 work 并给它挂任务（跨 work 用例要有一份属于别人的快照）。
func newTranscriptWork(t *testing.T, s *Server, st *store.Store, workID string) {
	t.Helper()
	ctx := context.Background()
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID: workID, Root: workID, Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Goal: "transcript 跨 work 测试", Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatalf("登记 work %s 失败: %v", workID, err)
	}
	task, err := st.OpenTask("conv-"+workID, "transcript 任务")
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if err := st.LinkWorkTask(ctx, workID, task.ID); err != nil {
		t.Fatalf("给 work %s 挂任务失败: %v", workID, err)
	}
}

// TestTranscriptRendersSnapshotInOrder 正常路径：按快照的消息顺序渲染，tool 行带 tool_call_id；
// 成功调用是只读的（快照数、触发状态都不动），重复调用结果一致。
func TestTranscriptRendersSnapshotInOrder(t *testing.T) {
	const workID = "w-transcript"
	s, st, taskID := newContextV2TestServer(t, workID)

	trigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "问题一", "", 1)
	insertContextV2Message(t, st, workID, taskID, "assistant", "assistant", "答复一", "", 2)
	insertContextV2Message(t, st, workID, taskID, "assistant", "tool_call", "pwd",
		`{"tool":"shell__run","call_id":"call-1","arguments":"{\"command\":\"pwd\"}"}`, 3)
	insertContextV2Message(t, st, workID, taskID, "tool", "tool_result", "工作目录",
		`{"call_id":"call-1"}`, 4)

	snap, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: trigger},
	})
	if err != nil {
		t.Fatalf("准备快照失败: %v", err)
	}
	if len(snap.Messages) == 0 {
		t.Fatal("测试构造失效：快照里没有消息")
	}

	res, err := callTranscript(t, s, protocol.TranscriptPayload{Work: workID, SnapshotID: snap.SnapshotID})
	if err != nil {
		t.Fatalf("transcript 失败: %v", err)
	}
	if res.Truncated || res.Omitted != 0 {
		t.Fatalf("无预算时不应截断: %+v", res)
	}
	for _, want := range []string{
		"user: 问题一",
		"assistant: 答复一",
		"tool: 工作目录 [tool_call_id=call-1]",
	} {
		if !strings.Contains(res.Text, want) {
			t.Fatalf("文本缺少 %q:\n%s", want, res.Text)
		}
	}
	// 顺序 = 快照里的消息顺序：问题一 → 答复一 → 工具结果。
	a, b, c := strings.Index(res.Text, "问题一"), strings.Index(res.Text, "答复一"), strings.Index(res.Text, "工作目录")
	if !(a >= 0 && a < b && b < c) {
		t.Fatalf("消息顺序不对（%d,%d,%d）:\n%s", a, b, c, res.Text)
	}

	// 只读：不产生第二份快照，不改触发状态。
	if n := countSnapshots(t, st); n != 1 {
		t.Fatalf("transcript 不应产生快照，实际 %d", n)
	}
	if state, snapshotID, err := st.GetTriggerState(context.Background(), workID, "user", trigger); err != nil ||
		state != store.TriggerStateAssembled || snapshotID != snap.SnapshotID {
		t.Fatalf("transcript 不应改触发状态: state=%q snapshot=%q err=%v", state, snapshotID, err)
	}
	// 幂等：同一请求两次结果逐字段相同。
	again, err := callTranscript(t, s, protocol.TranscriptPayload{Work: workID, SnapshotID: snap.SnapshotID})
	if err != nil {
		t.Fatalf("重复 transcript 失败: %v", err)
	}
	if again != res {
		t.Fatalf("transcript 应幂等:\nfirst=%+v\nsecond=%+v", res, again)
	}
}

// TestTranscriptUsesRequestedSnapshotNotLatest 文本必须对应调用方点名的那份快照：
// 同一 work 有两份快照时，要的是旧的那份就不能给最新的。
func TestTranscriptUsesRequestedSnapshotNotLatest(t *testing.T) {
	const workID = "w-transcript-two-snapshots"
	s, st, taskID := newContextV2TestServer(t, workID)

	firstTrigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "第一轮问题", "", 1)
	first, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: firstTrigger},
	})
	if err != nil {
		t.Fatalf("第一份快照失败: %v", err)
	}
	// 第二轮：新增消息后再组装，第二份快照包含更多历史。
	secondTrigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "第二轮问题", "", 2)
	second, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: secondTrigger},
	})
	if err != nil {
		t.Fatalf("第二份快照失败: %v", err)
	}
	if first.SnapshotID == second.SnapshotID {
		t.Fatalf("测试构造失效：两轮应有两份快照（%q）", first.SnapshotID)
	}

	oldRes, err := callTranscript(t, s, protocol.TranscriptPayload{Work: workID, SnapshotID: first.SnapshotID})
	if err != nil {
		t.Fatalf("按第一份快照取文本失败: %v", err)
	}
	if !strings.Contains(oldRes.Text, "第一轮问题") {
		t.Fatalf("第一份快照的文本缺少第一轮问题:\n%s", oldRes.Text)
	}
	if strings.Contains(oldRes.Text, "第二轮问题") {
		t.Fatalf("按第一份快照取文本却出现了第二轮内容（取成了最新快照）:\n%s", oldRes.Text)
	}

	newRes, err := callTranscript(t, s, protocol.TranscriptPayload{Work: workID, SnapshotID: second.SnapshotID})
	if err != nil {
		t.Fatalf("按第二份快照取文本失败: %v", err)
	}
	if !strings.Contains(newRes.Text, "第二轮问题") {
		t.Fatalf("第二份快照的文本缺少第二轮问题:\n%s", newRes.Text)
	}
}

// TestTranscriptBudgetTruncation 预算语义：max_chars 按字符截末尾，omitted 只数整条丢弃的。
func TestTranscriptBudgetTruncation(t *testing.T) {
	msgs := []protocol.LLMMessage{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "在"},
	}
	full := "user: 你好\nassistant: 在"
	if got := renderTranscript(msgs, 0); got.Text != full || got.Truncated || got.Omitted != 0 {
		t.Fatalf("无预算应完整输出: %+v", got)
	}

	// 恰好放得下：不算截断。
	if got := renderTranscript(msgs, utf8.RuneCountInString(full)); got.Text != full || got.Truncated {
		t.Fatalf("预算恰好等于全文不应截断: %+v", got)
	}

	// 放得下第一条、第二条只剩一个字符：第一条完整，第二条截成前缀 → omitted=0。
	if got := renderTranscript(msgs, 10); got.Text != "user: 你好\na" || !got.Truncated || got.Omitted != 0 {
		t.Fatalf("截在第二条中间时结果不对: %+v", got)
	}

	// 只放得下第一条（8 个字符）：第二条整条丢弃 → omitted=1。
	if got := renderTranscript(msgs, 8); got.Text != "user: 你好" || !got.Truncated || got.Omitted != 1 {
		t.Fatalf("整条丢弃时 omitted 不对: %+v", got)
	}

	// 第一条自己就放不下：留前缀，后面的整条丢弃 → omitted=1（被截的那条不算）。
	if got := renderTranscript(msgs, 3); got.Text != "use" || !got.Truncated || got.Omitted != 1 {
		t.Fatalf("首条截断时结果不对: %+v", got)
	}

	// 按 rune 切，不能切出半个汉字。
	cjk := renderTranscript(msgs[:1], 7)
	if cjk.Text != "user: 你" || !utf8.ValidString(cjk.Text) {
		t.Fatalf("按字符截断不应破坏 UTF-8: %q", cjk.Text)
	}

	// tool 行的 tool_call_id 后缀也计入预算，且格式固定。
	tool := []protocol.LLMMessage{{Role: "tool", Content: "ok", ToolCallID: "call-1"}}
	want := "tool: ok [tool_call_id=call-1]"
	if got := renderTranscript(tool, 0); got.Text != want {
		t.Fatalf("tool 行格式不对: %q，期望 %q", got.Text, want)
	}
	if got := renderTranscript(tool, utf8.RuneCountInString(want)-1); got.Omitted != 0 ||
		!got.Truncated || utf8.RuneCountInString(got.Text) != utf8.RuneCountInString(want)-1 {
		t.Fatalf("tool 行截断不对: %+v", got)
	}
}

// TestTranscriptBudgetWiring 预算经 handler 生效：输出字符数不超预算，且明确说被截了。
func TestTranscriptBudgetWiring(t *testing.T) {
	const workID = "w-transcript-budget"
	s, st, taskID := newContextV2TestServer(t, workID)
	trigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "问题一", "", 1)
	insertContextV2Message(t, st, workID, taskID, "assistant", "assistant", "答复一", "", 2)
	snap, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: trigger},
	})
	if err != nil {
		t.Fatalf("准备快照失败: %v", err)
	}

	const budget = 10
	res, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: workID, SnapshotID: snap.SnapshotID, Budget: protocol.TranscriptBudget{MaxChars: budget},
	})
	if err != nil {
		t.Fatalf("带预算的 transcript 失败: %v", err)
	}
	if !res.Truncated || res.Omitted <= 0 {
		t.Fatalf("预算 %d 下应有截断与整条丢弃（快照有 %d 条消息）: %+v", budget, len(snap.Messages), res)
	}
	if n := utf8.RuneCountInString(res.Text); n > budget {
		t.Fatalf("输出 %d 字符，超过预算 %d: %q", n, budget, res.Text)
	}
}

// TestTranscriptRejectsForeignSnapshot 归属：别的 work 的快照与「不存在」同形（not_found）；
// 帧头 work / 身份不匹配别人的 work 是 permission_denied；拒绝不产生副作用。
func TestTranscriptRejectsForeignSnapshot(t *testing.T) {
	const ownWork, otherWork = "w-transcript-own", "w-transcript-other"
	s, st, taskID := newContextV2TestServer(t, ownWork)
	newTranscriptWork(t, s, st, otherWork)

	otherTrigger := insertContextV2Message(t, st, otherWork, taskID, "user", "user", "别人的问题", "", 1)
	otherSnap, err := callContextV2(t, s, protocol.ContextRequest{
		Work: otherWork, Trigger: protocol.Trigger{Kind: "user", Item: otherTrigger},
	})
	if err != nil {
		t.Fatalf("准备别的 work 的快照失败: %v", err)
	}

	// 本 work 的合法身份去读别人 work 的快照：not_found（不能借错误码区分「存在但无权」）。
	if _, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: ownWork, SnapshotID: otherSnap.SnapshotID,
	}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("别的 work 的快照应 not_found，实际: %v", err)
	}

	// 载荷 work 直接指向别人的 work：帧头 work 不一致 → permission_denied（先于读快照）。
	req := protocol.TranscriptPayload{Work: otherWork, SnapshotID: otherSnap.SnapshotID}
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.transcript(context.Background(), body,
		caller{work: ownWork, from: spv2.AgentRef("reasoner")}); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("跨 work 应 permission_denied，实际: %v", err)
	}
	// 匿名一律拒绝（先于快照读取）。
	if _, err := s.transcript(context.Background(), body, caller{work: otherWork, from: spv2.Ref{}}); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("匿名应 permission_denied，实际: %v", err)
	}

	// 被拒绝的请求没有副作用：快照数不变，别人的触发状态仍是 assembled。
	if n := countSnapshots(t, st); n != 1 {
		t.Fatalf("被拒绝的请求不应产生 / 删除快照，实际 %d", n)
	}
	if state, _, err := st.GetTriggerState(context.Background(), otherWork, "user", otherTrigger); err != nil ||
		state != store.TriggerStateAssembled {
		t.Fatalf("别人的触发状态不应被改: state=%q err=%v", state, err)
	}
}

// TestTranscriptNotFoundAndExpired 快照缺失 / 过期 / work 不存在都回 not_found，且不得静默重建。
func TestTranscriptNotFoundAndExpired(t *testing.T) {
	const workID = "w-transcript-missing"
	s, st, taskID := newContextV2TestServer(t, workID)
	trigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	snap, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: trigger},
	})
	if err != nil {
		t.Fatalf("准备快照失败: %v", err)
	}

	if _, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: workID, SnapshotID: "no-such-snapshot",
	}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("缺失快照应 not_found，实际: %v", err)
	}
	expireSnapshot(t, st, workID, "user", trigger)
	if _, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: workID, SnapshotID: snap.SnapshotID,
	}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("过期快照应 not_found，实际: %v", err)
	}
	if _, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: "w-transcript-ghost", SnapshotID: snap.SnapshotID,
	}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("work 不存在应 not_found，实际: %v", err)
	}
	// 过期 / 缺失都只是读失败：不得借此重建快照或推进触发状态。
	if n := countSnapshots(t, st); n != 1 {
		t.Fatalf("失败路径不应改动快照，实际 %d", n)
	}
	if state, _, err := st.GetTriggerState(context.Background(), workID, "user", trigger); err != nil ||
		state != store.TriggerStateAssembled {
		t.Fatalf("失败路径不应改触发状态: state=%q err=%v", state, err)
	}
}

// TestTranscriptRejectsBadRequest body / work / snapshot_id / budget 的非法输入 → invalid_argument。
func TestTranscriptRejectsBadRequest(t *testing.T) {
	const workID = "w-transcript-bad"
	s, st, _ := newContextV2TestServer(t, workID)
	ctx := context.Background()
	good := caller{work: workID, from: spv2.AgentRef("reasoner")}

	if _, err := s.transcript(ctx, []byte{0xc1}, good); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("坏 body 应 invalid_argument，实际: %v", err)
	}
	for _, tc := range []struct {
		name string
		req  protocol.TranscriptPayload
	}{
		{"work 为空", protocol.TranscriptPayload{Work: "  ", SnapshotID: "snap-1"}},
		{"snapshot_id 为空", protocol.TranscriptPayload{Work: workID, SnapshotID: " "}},
		{"budget 为负", protocol.TranscriptPayload{Work: workID, SnapshotID: "snap-1",
			Budget: protocol.TranscriptBudget{MaxChars: -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := protocol.EncodePayload(tc.req)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.transcript(ctx, body, good); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
				t.Fatalf("应 invalid_argument，实际: %v", err)
			}
		})
	}
	// 非法请求不落任何东西。
	if n := countSnapshots(t, st); n != 0 {
		t.Fatalf("非法请求不应产生快照，实际 %d", n)
	}
}

// TestTranscriptCorruptPayloadIsInternal 解压 / 解码失败回 internal：坏数据不能变成空文本。
func TestTranscriptCorruptPayloadIsInternal(t *testing.T) {
	const workID = "w-transcript-corrupt"
	s, st, taskID := newContextV2TestServer(t, workID)
	trigger := insertContextV2Message(t, st, workID, taskID, "user", "user", "触发", "", 1)
	snap, err := callContextV2(t, s, protocol.ContextRequest{
		Work: workID, Trigger: protocol.Trigger{Kind: "user", Item: trigger},
	})
	if err != nil {
		t.Fatalf("准备快照失败: %v", err)
	}

	// 1) 载荷不是 gzip：解压失败。
	if _, err := st.DB().Exec(`UPDATE context_snapshots SET payload = ? WHERE snapshot_id = ?`,
		[]byte("not-gzip"), snap.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: workID, SnapshotID: snap.SnapshotID,
	}); !spv2.IsCode(err, spv2.CodeInternal) {
		t.Fatalf("解压失败应 internal，实际: %v", err)
	}

	// 2) gzip 合法但载荷不是 ContextResponse：解码失败。
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte{0xc1}); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE context_snapshots SET payload = ? WHERE snapshot_id = ?`,
		buf.Bytes(), snap.SnapshotID); err != nil {
		t.Fatal(err)
	}
	if _, err := callTranscript(t, s, protocol.TranscriptPayload{
		Work: workID, SnapshotID: snap.SnapshotID,
	}); !spv2.IsCode(err, spv2.CodeInternal) {
		t.Fatalf("解码失败应 internal，实际: %v", err)
	}
}

// TestTranscriptMethodDeclaration 注册声明：非工具、输入必填 work/snapshot_id、输出三字段齐全。
func TestTranscriptMethodDeclaration(t *testing.T) {
	m := transcriptMethod()
	if m.Name != "transcript" {
		t.Fatalf("method 名不符（期望 transcript，实际 %q）", m.Name)
	}
	if m.Tool {
		t.Fatal("transcript 不应暴露给模型（Tool 必须为 false）")
	}
	if m.Input == nil || m.Input["type"] != "object" || m.Output["type"] != "object" {
		t.Fatalf("input/output 必须是 object schema: %+v / %+v", m.Input, m.Output)
	}
	for _, field := range []string{"work", "snapshot_id"} {
		if !schemaRequires(m.Input, field) {
			t.Errorf("transcript 的 input 应必填 %s: %+v", field, m.Input)
		}
	}
	if !schemaHasProperty(m.Input, "budget") {
		t.Errorf("transcript 的 input 应声明 budget: %+v", m.Input)
	}
	for _, field := range []string{"text", "truncated", "omitted"} {
		if !schemaRequires(m.Output, field) {
			t.Errorf("transcript 的 output 应必填 %s: %+v", field, m.Output)
		}
	}

	// 注册本身可调用（重复注册是覆盖，不 panic）。
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
	s.registerTranscript()
	s.registerTranscript()
}
