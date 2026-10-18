package server

import (
	"strings"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// TestTranscriptSerializesRoles 验证流水把四种东西分开渲染：
// 用户的要求、助手的发言、工具调用、工具输出（在黑板里工具结果也是 role=user）。
func TestTranscriptSerializesRoles(t *testing.T) {
	msgs := []llm.Message{
		{Role: "system", Content: "你是脚手架，不该出现在流水里"},
		{Role: "user", Content: "把端口改成 9000"},
		{Role: "assistant", Content: "我先看看配置", ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: "shell__exec", Arguments: `{"cmd":"cat app.yaml"}`}},
		}},
		{Role: "user", Content: "[工具结果] shell__exec（job=c1）：\nport: 8080"},
		{Role: "assistant", Content: "改好了"},
	}
	got, _ := buildTranscript(msgs, "端口已改成 9000")

	for _, want := range []string{"[用户]: 把端口改成 9000", "[助手]: 我先看看配置", "[工具调用]: shell__exec(", "[工具输出]: ", "[最终答复]: 端口已改成 9000"} {
		if !strings.Contains(got, want) {
			t.Fatalf("流水缺 %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "脚手架") {
		t.Fatalf("系统提示不该进流水（那是我们的脚手架，不是干了什么）:\n%s", got)
	}
}

// TestTranscriptTruncatesToolOutput 验证单条工具输出被截断：
// 它的体积能顶掉整段对话，砍它收益最大（opencode/codex 同为 2000 字）。
func TestTranscriptTruncatesToolOutput(t *testing.T) {
	huge := strings.Repeat("错", 10_000)
	got, _ := buildTranscript([]llm.Message{
		{Role: "user", Content: "跑一下测试"},
		{Role: "user", Content: "[工具结果] shell__exec（job=c1）：\n" + huge},
	}, "")
	if n := len([]rune(got)); n > reflectToolOutputChars+200 {
		t.Fatalf("工具输出没被截断：整段 %d 字", n)
	}
	if !strings.Contains(got, "…") {
		t.Fatalf("截断应留明确的省略号:\n%s", got[:120])
	}
}

// TestTranscriptSkeletonizesOldPart 验证"最近的逐字保留、更早的骨架化"：
// 老的助手长文只留开头，老的工具输出只剩一行"已省略"。
func TestTranscriptSkeletonizesOldPart(t *testing.T) {
	oldAssistant := strings.Repeat("旧结论", 4_000) // ≈12k token，必然落在窗口外
	msgs := []llm.Message{
		{Role: "user", Content: "很久以前的需求"},
		{Role: "assistant", Content: oldAssistant},
		{Role: "user", Content: "[工具结果] t（job=c0）：\n" + strings.Repeat("旧输出", 2_500)},
		// 这一条把保留窗口（8k）占满，让它前面的三条全部落到窗口外。
		{Role: "assistant", Content: strings.Repeat("近况", 4_000)},
		{Role: "user", Content: "刚刚才说的话，必须原样保留"},
	}

	got, _ := buildTranscript(msgs, "")
	if !strings.Contains(got, "刚刚才说的话，必须原样保留") {
		t.Fatalf("最近的内容必须逐字保留:\n%s", clip(got, 300))
	}
	if !strings.Contains(got, "（输出已省略）") {
		t.Fatalf("老的工具输出应骨架化成「输出已省略」:\n%s", clip(got, 300))
	}
	if strings.Contains(got, strings.Repeat("旧结论", 1_000)) {
		t.Fatalf("老的助手长文应被截短")
	}
}

// TestTranscriptDropsOldest 验证硬上限：整段超预算就从最老的丢，并在开头写明丢了几条。
func TestTranscriptDropsOldest(t *testing.T) {
	var msgs []llm.Message
	for i := 0; i < 200; i++ {
		msgs = append(msgs, llm.Message{Role: "user", Content: strings.Repeat("很长的需求描述", 200)})
	}
	msgs = append(msgs, llm.Message{Role: "assistant", Content: "最后一句"})

	got, tokens := buildTranscript(msgs, "")
	if tokens > reflectTotalTokens*2 {
		t.Fatalf("硬上限失效：%d token", tokens)
	}
	if !strings.Contains(got, "条记录已省略") {
		t.Fatalf("丢内容时必须在开头写明丢了什么:\n%s", clip(got, 200))
	}
	if !strings.Contains(got, "最后一句") {
		t.Fatalf("丢掉的一定是最老的，最后一句必须还在")
	}
}

// TestParseNotes 验证复盘输出的解析：容忍围栏、拒绝空内容、偏好才升 core、最多 3 条。
func TestParseNotes(t *testing.T) {
	raw := "```json\n{\"notes\":[" +
		`{"content":"做法A","kind":"做法"},` +
		`{"content":"  ","kind":"做法"},` +
		`{"content":"偏好B","kind":"偏好"},` +
		`{"content":"坑C","kind":"坑"},` +
		`{"content":"取舍D","kind":"取舍"}` +
		"]}\n```"
	notes := parseNotes(raw)
	if len(notes) != reflectMaxNotes {
		t.Fatalf("应截到 %d 条: %+v", reflectMaxNotes, notes)
	}
	if notes[0].Content != "做法A" || notes[0].Durability != "durable" {
		t.Fatalf("做法应 durable: %+v", notes[0])
	}
	if notes[1].Content != "偏好B" || notes[1].Durability != "core" {
		t.Fatalf("偏好应 core（不该忘）: %+v", notes[1])
	}

	// 空内容被跳过；全空 = 什么都不写（宁缺勿滥）。
	if got := parseNotes(`{"notes":[{"content":"","kind":"坑"}]}`); len(got) != 0 {
		t.Fatalf("空内容应被丢弃: %+v", got)
	}
	// 模型胡说八道时也不要抛错，当没写。
	if got := parseNotes("我觉得这次干得不错"); len(got) != 0 {
		t.Fatalf("解析不了就当没写: %+v", got)
	}
}
