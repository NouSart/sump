package tests

// 全局名册只有 memory 一张卡：session / experience 是独立模式，不得注册。

import "testing"

func TestOnlyFacadeRegistersToCenter(t *testing.T) {
	sc := startStubCenter(t)
	_ = startMemory(t, sc.URL(), 12)

	sc.mu.Lock()
	defer sc.mu.Unlock()
	if _, ok := sc.cards["session"]; ok {
		t.Fatal("session 不得注册到设置中心")
	}
	if _, ok := sc.cards["experience"]; ok {
		t.Fatal("experience 不得注册到设置中心")
	}
	if card, ok := sc.cards["memory"]; !ok || card.Name != "memory" {
		t.Fatalf("名册应只有 memory: %+v", sc.cards)
	}
}
