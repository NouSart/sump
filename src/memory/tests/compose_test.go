package tests

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/facade/compose"
	"github.com/NouSart/sump/protocol"
)

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

// TestInject 验证记忆块的插入 / 替换 / 移除（基座消息是 LLMMessage）。
func TestInject(t *testing.T) {
	base := []protocol.LLMMessage{{Role: "system", Content: "sys"}, {Role: "user", Content: "u"}}

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

	out4 := compose.Inject([]protocol.LLMMessage{{Role: "user", Content: "u"}}, "m")
	if len(out4) != 2 || out4[0].Role != "system" || !strings.Contains(out4[0].Content, "m") {
		t.Fatalf("无 system 时应置顶: %+v", out4)
	}
}
