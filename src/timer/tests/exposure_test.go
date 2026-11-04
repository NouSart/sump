package tests

// 工具暴露面：sleep（异步）与 cancel 给模型；list 只给门户，不得被模型看见。

import (
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/timer/server"
)

func TestToolExposure(t *testing.T) {
	sc := startStubCenter(t)
	_ = startTimer(t, sc, server.Config{})

	sc.mu.Lock()
	card := sc.cards["timer"]
	sc.mu.Unlock()
	if card.Name != "timer" {
		t.Fatalf("未找到 timer 名片: %+v", card)
	}

	byAction := map[string]protocol.Provide{}
	for _, p := range card.Provides {
		byAction[p.Action] = p
	}
	sleep, ok := byAction["sleep"]
	if !ok || !sleep.Tool || !sleep.Async {
		t.Fatalf("sleep 应是 Tool:true + Async:true: %+v", sleep)
	}
	cancel, ok := byAction["cancel"]
	if !ok || !cancel.Tool {
		t.Fatalf("cancel 应暴露给模型: %+v", cancel)
	}
	if list, ok := byAction["list"]; !ok || list.Tool {
		t.Fatalf("list 必须存在且只给门户，不暴露给模型: %+v", list)
	}
}
