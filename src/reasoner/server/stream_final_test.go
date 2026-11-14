package server

import (
	"testing"

	"github.com/NouSart/sump/reasoner/llm"
)

func TestFinalAssistantText(t *testing.T) {
	msgs := []llm.Message{
		{Role: "user", Content: "hi"},
		{Role: "assistant", Content: "工具前草稿", ToolCalls: []llm.ToolCall{{ID: "c1"}}},
		{Role: "tool", Content: "result"},
		{Role: "assistant", Content: "最终答案"},
	}
	if got := finalAssistantText(msgs); got != "最终答案" {
		t.Fatalf("终态应取最后一条纯文本 assistant，实际 %q", got)
	}
	if got := finalAssistantText(nil); got != "" {
		t.Fatalf("空黑板应返回空串，实际 %q", got)
	}
}
