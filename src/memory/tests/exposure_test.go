package tests

// 工具暴露面：provides 里只有 dig / remember / shared_sessions 标了 Tool:true，
// 其余动作（recall / store / history / 发布会话摘要 …）不得被模型看见。
// 改错一个 flag，模型就会多一个不该有的工具；这里钉住。

import (
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestToolExposure(t *testing.T) {
	sc := startStubCenter(t)
	_ = startMemory(t, sc.URL(), 12)

	sc.mu.Lock()
	card := sc.cards["memory"]
	sc.mu.Unlock()
	if card.Name != "memory" {
		t.Fatalf("未找到 memory 名片: %+v", card)
	}

	tools := map[string]protocol.Provide{}
	for _, p := range card.Provides {
		if p.Tool {
			tools[p.Action] = p
		}
	}
	if len(tools) != 3 {
		t.Fatalf("应只暴露 3 个工具，实际 %v", tools)
	}
	for _, want := range []string{"dig", "remember", "shared_sessions"} {
		if _, ok := tools[want]; !ok {
			t.Fatalf("memory.%s 没暴露给模型（Tool: true 缺失）; 实际暴露=%v", want, tools)
		}
	}
	for _, notWant := range []string{
		"recall", "store", "history", "conversation_list", "conversation_rename", "note", "lineheads", "prune",
		"publish_session", "shared_forget", "conversation_open", "conversation_archive",
	} {
		if _, ok := tools[notWant]; ok {
			t.Fatalf("memory.%s 不应作为工具暴露", notWant)
		}
	}
}
