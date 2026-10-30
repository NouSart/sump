package tests

// 工具暴露面：run 是给模型的异步工具；审批与执行反馈走别的通道，不在 provides 里多露动作。

import (
	"testing"

	"github.com/NouSart/sump/shell/server"
)

func TestToolExposure(t *testing.T) {
	sc := startStubCenter(t)
	_ = startShell(t, sc, server.Config{})

	sc.mu.Lock()
	card := sc.cards["shell"]
	sc.mu.Unlock()
	if card.Name != "shell" {
		t.Fatalf("未找到 shell 名片: %+v", card)
	}

	if len(card.Provides) != 1 {
		t.Fatalf("shell 应只声明 run 一个动作: %+v", card.Provides)
	}
	run := card.Provides[0]
	if run.Action != "run" || !run.Tool || !run.Async {
		t.Fatalf("run 应是 Tool:true + Async:true: %+v", run)
	}
}
