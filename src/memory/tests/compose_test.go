package tests

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/compose"
	"github.com/NouSart/sump/protocol"
)

// TestConversationTail 验证任务上下文尾部对话消息的提取边界。
func TestConversationTail(t *testing.T) {
	msgs := []protocol.Message{
		{Role: "system", Content: "sys"},
		{Role: "system", Content: compose.MemoryPrefix + "\n旧的记忆"},
		{Role: "user", Content: "u"},
		{Role: "assistant", ToolCalls: []protocol.ToolCall{{ID: "1", Type: "function"}}},
		{Role: "tool", Content: "t", ToolCallID: "1"},
	}
	tail := compose.ConversationTail(msgs)
	if len(tail) != 1 || tail[0].Content != "u" {
		t.Fatalf("tail 不符: %+v", tail)
	}
	// 旧记忆块之后不再往前收集（任务链边界）。
	if tail[0].Role != "user" {
		t.Fatalf("tail[0] 角色不符: %+v", tail[0])
	}
}

// TestTrimOverlap 验证历史尾部与任务链重复部分的裁剪。
func TestTrimOverlap(t *testing.T) {
	hist := []protocol.Message{{Role: "user", Content: "a"}, {Role: "assistant", Content: "b"}}
	got := compose.TrimOverlap(hist, []protocol.Message{{Role: "assistant", Content: "b"}})
	if len(got) != 1 || got[0].Content != "a" {
		t.Fatalf("裁剪不符: %+v", got)
	}
	got2 := compose.TrimOverlap(hist, []protocol.Message{{Role: "user", Content: "z"}})
	if len(got2) != 2 {
		t.Fatalf("无重叠不应裁剪: %+v", got2)
	}
}

// TestBuildText 验证记忆文本的拼接。
func TestBuildText(t *testing.T) {
	text := compose.BuildText([]protocol.Message{
		{Role: "user", Content: "你好"},
		{Role: "assistant", Content: "你好呀"},
		{Role: "tool", Content: "忽略我"},
	})
	if !strings.Contains(text, "用户: 你好") || !strings.Contains(text, "助手: 你好呀") || strings.Contains(text, "忽略我") {
		t.Fatalf("BuildText 不符: %q", text)
	}
}

// TestInject 验证记忆块的插入 / 替换 / 移除。
func TestInject(t *testing.T) {
	base := []protocol.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "u"}}

	out := compose.Inject(base, "hist")
	if len(out) != 3 || out[1].Role != "system" || !strings.HasPrefix(out[1].Content, compose.MemoryPrefix) || !strings.Contains(out[1].Content, "hist") {
		t.Fatalf("插入不符: %+v", out)
	}

	out2 := compose.Inject(out, "new")
	if len(out2) != 3 || strings.Contains(out2[1].Content, "hist") || !strings.Contains(out2[1].Content, "new") {
		t.Fatalf("替换不符: %+v", out2)
	}

	out3 := compose.Inject(out, "")
	if len(out3) != 2 || out3[1].Content != "u" {
		t.Fatalf("空记忆应移除旧块: %+v", out3)
	}

	out4 := compose.Inject([]protocol.Message{{Role: "user", Content: "u"}}, "m")
	if len(out4) != 2 || out4[0].Role != "system" || !strings.Contains(out4[0].Content, "m") {
		t.Fatalf("无 system 时应置顶: %+v", out4)
	}
}
