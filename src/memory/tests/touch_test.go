package tests

// context 注入后的访问计数：只有真正随上下文发出去的条目才累加。

import (
	"testing"

	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/protocol"
)

func TestContextTouchesInjectedMemories(t *testing.T) {
	sc := startStubCenter(t)
	var sh *shared.Store
	srv := startMemory(t, sc.URL(), 12, func(c *stackConfig) {
		sh = c.Shared
	})
	if err := callMemory(t, srv.WsURL(), "conversation_open", protocol.ConversationOpenPayload{ID: "c-touch", Entry: "stub-entry"}, nil); err != nil {
		t.Fatal(err)
	}

	var cat, city protocol.RememberResult
	if err := callMemory(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "我养了一只猫", Salience: 0.5}, &cat); err != nil {
		t.Fatal(err)
	}
	if err := callMemory(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "我住在杭州"}, &city); err != nil {
		t.Fatal(err)
	}

	in := protocol.ContextPayload{
		Conversation: "c-touch",
		Boss:         "stub-entry",
		Messages:     []protocol.Message{{Role: "user", Content: "我养了一只猫"}},
	}
	if err := callMemoryFrom(t, srv.WsURL(), "reasoner", "context", in, nil); err != nil {
		t.Fatalf("context 失败: %v", err)
	}

	entries, err := sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	hit, ok := findEntry(entries, "我养了一只猫")
	if !ok || hit.AccessCount != 1 || hit.LastAccessAt <= 0 {
		t.Fatalf("被注入的条目应记一次访问: %+v", hit)
	}
	if miss, ok := findEntry(entries, "我住在杭州"); !ok || miss.AccessCount != 0 {
		t.Fatalf("未被注入的条目不应计数: %+v", miss)
	}
}
