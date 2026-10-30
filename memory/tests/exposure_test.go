package tests

// 工具暴露面：provides 里只有 dig / remember 标了 Tool:true，其余动作（recall / store / history …）
// 不得被模型看见。改错一个 flag，模型就会多一个不该有的工具；这里钉住。

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
	if len(tools) != 2 {
		t.Fatalf("应只暴露 2 个工具，实际 %v", tools)
	}
	for _, want := range []string{"dig", "remember"} {
		if _, ok := tools[want]; !ok {
			t.Fatalf("memory.%s 没暴露给模型（Tool: true 缺失）; 实际暴露=%v", want, tools)
		}
	}
	for _, notWant := range []string{"recall", "store", "history", "conversation_list", "conversation_rename", "note", "lineheads", "prune"} {
		if _, ok := tools[notWant]; ok {
			t.Fatalf("memory.%s 不应作为工具暴露", notWant)
		}
	}
}
