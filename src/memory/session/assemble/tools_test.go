package assemble

import (
	"strings"
	"testing"
)

// toolCall 造一条工具调用记录。
func toolCall(itemID, callID, name, args string, seq int64) Record {
	return Record{
		ItemID: itemID, Role: "assistant", Kind: "tool_call", Content: name + " " + args,
		WorkID: "w1", CallID: callID, Seq: seq, Name: name, Arguments: args,
	}
}

// toolResult 造一条工具结果记录。
func toolResult(itemID, callID, content string, seq int64) Record {
	return Record{
		ItemID: itemID, Role: "tool", Kind: "tool_result", Content: content,
		WorkID: "w1", CallID: callID, Seq: seq,
	}
}

func TestToolPairingOutOfOrderResults(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{
		toolCall("tc1", "call-1", "shell__run", `{"command":"ls"}`, 1),
		toolCall("tc2", "call-2", "timer__sleep", `{"ms":100}`, 2),
	}
	// 结果故意乱序：call-2 的结果先到。
	in.ToolResults = []Record{
		toolResult("tr2", "call-2", "睡完了", 4),
		toolResult("tr1", "call-1", "file.txt", 3),
	}
	out := mustAssemble(t, in)

	if len(out.Messages) != 5 {
		t.Fatalf("应得到 system + 2*(assistant+tool)，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	// 配对顺序跟随 ToolCalls 的记录顺序（call-1 在前）。
	if m := out.Messages[1]; m.Role != "assistant" || len(m.ToolCalls) != 1 ||
		m.ToolCalls[0].ID != "call-1" || m.ToolCalls[0].Name != "shell__run" ||
		m.ToolCalls[0].Arguments != `{"command":"ls"}` {
		t.Fatalf("messages[1] 应是 call-1 的 assistant.tool_calls，得到 %+v", m)
	}
	if m := out.Messages[2]; m.Role != "tool" || m.ToolCallID != "call-1" || m.Content != "file.txt" {
		t.Fatalf("messages[2] 应是 call-1 的结果（按 call_id 配对，不看切片顺序），得到 %+v", m)
	}
	if m := out.Messages[3]; m.Role != "assistant" || len(m.ToolCalls) != 1 || m.ToolCalls[0].ID != "call-2" {
		t.Fatalf("messages[3] 应是 call-2 的 assistant.tool_calls，得到 %+v", m)
	}
	if m := out.Messages[4]; m.Role != "tool" || m.ToolCallID != "call-2" || m.Content != "睡完了" {
		t.Fatalf("messages[4] 应是 call-2 的结果，得到 %+v", m)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("正常配对不该有 warning: %+v", out.Warnings)
	}
	if got := findProv(out, "tool_call"); len(got) != 2 {
		t.Fatalf("每次调用应有一条 provenance: %+v", got)
	}
	if got := findProv(out, "tool_result"); len(got) != 2 {
		t.Fatalf("每条结果应有一条 provenance: %+v", got)
	}
}

func TestToolOutputTruncatedWithWarning(t *testing.T) {
	in := testInput()
	in.Config.MaxToolOutputChars = 5
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", "", 1)}
	in.ToolResults = []Record{toolResult("tr1", "call-1", "0123456789", 2)}
	out := mustAssemble(t, in)

	tool := out.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call-1" {
		t.Fatalf("结果消息形态不对: %+v", tool)
	}
	if want := "01234" + TruncationMark; tool.Content != want {
		t.Fatalf("截断结果 = %q，want %q", tool.Content, want)
	}
	if !hasWarning(out, "工具结果 call-1 超出 5 字，已截断") {
		t.Fatalf("截断应 warning: %+v", out.Warnings)
	}
}

func TestToolOutputTruncationKeepsUTF8(t *testing.T) {
	in := testInput()
	in.Config.MaxToolOutputChars = 2
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", "", 1)}
	in.ToolResults = []Record{toolResult("tr1", "call-1", "中文输出很长", 2)}
	out := mustAssemble(t, in)
	if want := "中文" + TruncationMark; out.Messages[2].Content != want {
		t.Fatalf("UTF-8 截断 = %q，want %q", out.Messages[2].Content, want)
	}
}

func TestInflightPlaceholderDoesNotBlock(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", `{}`, 1)}
	in.Inflight = []Inflight{{CallID: "call-1", Service: "shell", Action: "run", State: "running"}}
	out := mustAssemble(t, in)

	tool := out.Messages[2]
	if tool.Role != "tool" || tool.ToolCallID != "call-1" {
		t.Fatalf("在途占位必须是配对的 role=tool 消息: %+v", tool)
	}
	if !strings.Contains(tool.Content, "仍在运行") {
		t.Fatalf("在途占位文本应说明仍在运行: %q", tool.Content)
	}
	if strings.Contains(tool.Content, "结果未知") {
		t.Fatalf("在途不是“结果未知”: %q", tool.Content)
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("在途是正常状态，不该 warning: %+v", out.Warnings)
	}
	if got := findProv(out, "inflight"); len(got) != 1 || got[0].Ref != "call-1" {
		t.Fatalf("在途 provenance 不符: %+v", got)
	}
}

func TestUnknownResultPlaceholderWarns(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", `{}`, 1)}
	out := mustAssemble(t, in)

	tool := out.Messages[2]
	if !strings.Contains(tool.Content, "结果未知") {
		t.Fatalf("无结果且不在途必须给“结果未知”占位: %q", tool.Content)
	}
	if !hasWarning(out, "工具调用 call-1 结果未知") {
		t.Fatalf("结果未知应 warning: %+v", out.Warnings)
	}
	if got := findProv(out, "unknown"); len(got) != 1 {
		t.Fatalf("unknown provenance 不符: %+v", got)
	}
}

func TestOrphanToolResultDroppedWithWarning(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", "", 1)}
	in.ToolResults = []Record{
		toolResult("tr1", "call-1", "正常结果", 2),
		toolResult("tr9", "call-9", "找不到调用", 3),
		toolResult("tr0", "", "没有 call_id", 4),
	}
	out := mustAssemble(t, in)

	// system + assistant + tool = 3 条；孤立结果不得进入 messages。
	if len(out.Messages) != 3 {
		t.Fatalf("孤立结果不得进入 messages，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	for _, m := range out.Messages {
		if strings.Contains(m.Content, "找不到调用") || strings.Contains(m.Content, "没有 call_id") {
			t.Fatalf("孤立结果内容泄漏进 messages: %+v", m)
		}
	}
	if !hasWarning(out, "孤立的工具结果 tr9") || !hasWarning(out, "孤立的工具结果 tr0") {
		t.Fatalf("每个孤立结果都要 warning: %+v", out.Warnings)
	}
	got := findProv(out, "orphan")
	if len(got) != 2 || got[0].Index != -1 || got[1].Index != -1 {
		t.Fatalf("孤立结果应只留 provenance（Index=-1）: %+v", got)
	}
}

func TestDuplicateToolResultsUseEarliest(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", "", 1)}
	in.ToolResults = []Record{
		toolResult("tr1", "call-1", "第一次", 2),
		toolResult("tr2", "call-1", "第二次", 3),
	}
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 {
		t.Fatalf("重复结果只产生一条 tool 消息，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if out.Messages[2].Content != "第一次" {
		t.Fatalf("应使用 Seq 最早的结果，得到 %q", out.Messages[2].Content)
	}
	if !hasWarning(out, "有 2 条结果，只用最早一条") {
		t.Fatalf("重复结果应 warning: %+v", out.Warnings)
	}
}

func TestDuplicateToolCallIgnoredWithWarning(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{
		toolCall("tc1", "call-1", "shell__run", "", 1),
		toolCall("tc1b", "call-1", "shell__run", "", 2),
	}
	in.ToolResults = []Record{toolResult("tr1", "call-1", "ok", 3)}
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 {
		t.Fatalf("重复调用只渲染一次，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if !hasWarning(out, "工具调用 call-1 重复出现") {
		t.Fatalf("重复调用应 warning: %+v", out.Warnings)
	}
}

func TestMissingCallIDStillProducesPairedPlaceholder(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{toolCall("tc1", "", "shell__run", "", 1)}
	out := mustAssemble(t, in)
	// system + assistant + tool：没有 call_id 也必须占住配对位，不能静默消失。
	if len(out.Messages) != 3 {
		t.Fatalf("缺 call_id 也要产生配对消息，实际 %d 条: %+v", len(out.Messages), out.Messages)
	}
	if len(out.Messages[1].ToolCalls) != 1 || out.Messages[1].ToolCalls[0].ID != "" {
		t.Fatalf("assistant.tool_calls 缺 call_id 时原样保留空 id: %+v", out.Messages[1])
	}
	if out.Messages[2].ToolCallID != "" {
		t.Fatalf("tool 消息的 ToolCallID 应与调用一致（空）: %+v", out.Messages[2])
	}
	if !hasWarning(out, "结果未知") {
		t.Fatalf("缺 call_id 无法配对，应 warning 结果未知: %+v", out.Warnings)
	}
}

func TestToolNameFallbackFromInflight(t *testing.T) {
	in := testInput()
	// 投影没填 Name，但在途记录知道 service/action：用 shell__run 兜底，而不是 unknown。
	in.ToolCalls = []Record{{ItemID: "tc1", Role: "assistant", Kind: "tool_call", WorkID: "w1", CallID: "call-1", Seq: 1}}
	in.Inflight = []Inflight{{CallID: "call-1", Service: "shell", Action: "run", State: "running"}}
	out := mustAssemble(t, in)
	if got := out.Messages[1].ToolCalls[0].Name; got != "shell__run" {
		t.Fatalf("缺少工具名时应从在途记录兜底，得到 %q", got)
	}
	if hasWarning(out, "缺少工具名") {
		t.Fatalf("兜底成功不该 warning: %+v", out.Warnings)
	}
}

func TestToolCallWithoutNameWarns(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{{ItemID: "tc1", Role: "assistant", Kind: "tool_call", WorkID: "w1", CallID: "call-1", Seq: 1}}
	out := mustAssemble(t, in)
	if got := out.Messages[1].ToolCalls[0].Name; got != "unknown" {
		t.Fatalf("没有任何线索时工具名应为 unknown，得到 %q", got)
	}
	if !hasWarning(out, "缺少工具名") {
		t.Fatalf("工具名缺失应 warning: %+v", out.Warnings)
	}
}

func TestToolResultFromOtherWorkNeedsGrant(t *testing.T) {
	in := testInput()
	in.ToolCalls = []Record{toolCall("tc1", "call-1", "shell__run", "", 1)}
	in.ToolResults = []Record{{
		ItemID: "tr1", Role: "tool", Kind: "tool_result", WorkID: "w2",
		CallID: "call-1", Content: "别人的结果", Seq: 2,
	}}

	// 没有 grant：结果被过滤 → 调用变成“结果未知”。
	out := mustAssemble(t, in)
	if len(out.Messages) != 3 || !strings.Contains(out.Messages[2].Content, "结果未知") {
		t.Fatalf("无授权的跨 work 结果应被过滤: %+v", out.Messages)
	}
	if got := findProv(out, "filtered"); len(got) != 1 || got[0].Ref != "tr1" {
		t.Fatalf("被过滤的结果应留 filtered provenance: %+v", got)
	}

	// 用 call_id 显式授权：结果可见并与调用配对。
	in.Grants = []Grant{{RefKind: "message", RefID: "call-1"}}
	out = mustAssemble(t, in)
	if len(out.Messages) != 3 || out.Messages[2].Content != "别人的结果" || out.Messages[2].ToolCallID != "call-1" {
		t.Fatalf("grant 后跨 work 结果应可见并配对: %+v", out.Messages)
	}
}

func TestNoCallsAllResultsOrphaned(t *testing.T) {
	in := testInput()
	in.ToolResults = []Record{toolResult("tr1", "call-1", "结果", 1)}
	out := mustAssemble(t, in)
	if len(out.Messages) != 1 {
		t.Fatalf("没有调用时结果不得进入 messages: %+v", out.Messages)
	}
	if !hasWarning(out, "孤立的工具结果 tr1") {
		t.Fatalf("应 warning 孤立结果: %+v", out.Warnings)
	}
}
