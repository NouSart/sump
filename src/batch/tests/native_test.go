package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// TestNativeSPV2MethodsRegistered：batch 服务卡必须把 Provides 的五个动作都声明成
// 原生 method（带 input/output schema），而不是让骨架用通用桥接 method 顶替。
func TestNativeSPV2MethodsRegistered(t *testing.T) {
	t.Setenv("SUMP_SPV2", "1")
	f := startFixture(t, nil)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr := strings.TrimSuffix(f.batch.WsURL(), "/ws") + "/spv2"
	c, err := spv2.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", addr, err)
	}
	t.Cleanup(func() { _ = c.Close() })

	card, err := c.Hello(ctx, spv2.ServiceRef("probe"), spv2.ServiceRef("batch"))
	if err != nil {
		t.Fatalf("hello 失败: %v", err)
	}
	for _, name := range []string{"run", "status", "result", "list", "control"} {
		m, ok := card.Method(name)
		if !ok {
			t.Fatalf("服务卡里没有 method %q", name)
		}
		if !m.Tool {
			t.Fatalf("%s 应声明为 tool", name)
		}
		if m.Input["properties"] == nil {
			t.Fatalf("%s 应带具体 input schema，实际 %v", name, m.Input)
		}
		if m.Output["properties"] == nil {
			t.Fatalf("%s 应带具体 output schema，实际 %v", name, m.Output)
		}
	}
	run, _ := card.Method("run")
	if !run.LongRunning {
		t.Fatal("run 是异步动作，应声明 long_running")
	}

	// 再直连 /spv2 调一次 status：错误文本必须与 v1 边一致，证明原生 handler 真的在接客。
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef("probe"), spv2.ServiceRef("batch"),
		"status", protocol.BatchStatusPayload{ItemID: "01ZZZZZZZZZZZZZZZZZZZZZZZZ"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call); err == nil || !strings.Contains(err.Error(), "BATCH_JOB_UNKNOWN") {
		t.Fatalf("status 应经原生 handler 回 BATCH_JOB_UNKNOWN，实际 %v", err)
	}
}
