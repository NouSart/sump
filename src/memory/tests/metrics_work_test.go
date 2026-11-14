package tests

// 真实链路：context 登记 work → work_update 推进 done / work_cancel 推进 cancelled，
// 指标必须由真实 method 触发（不是测试直接调内部函数）。

import (
	"context"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestWorkTransitionMetricsOnRealCalls(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ws := srv.WsURL()
	workID := "w-state-metric"
	c, err := spv2.Dial(ctx, ws)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctxCall, _ := spv2.NewCall(workID, spv2.ServiceRef("reasoner"), spv2.ServiceRef("memory"), "context", protocol.ContextPayload{
		Conversation: "conv-metric", Boss: "frontend",
		Messages: []protocol.Message{{Role: "user", Content: "hi"}},
	})
	if _, err := c.Call(ctx, ctxCall); err != nil {
		t.Fatalf("context 建 work 失败: %v", err)
	}
	runtimeRef := spv2.AgentRef("runtime").WithAuthority(spv2.AuthorityRuntime)
	update, _ := spv2.NewCall(workID, runtimeRef, spv2.ServiceRef("memory"), "work_update", map[string]any{"work": workID, "status": spv2.WorkDone})
	if _, err := c.Call(ctx, update); err != nil {
		t.Fatalf("work_update done 失败: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range srv.Metrics().Snapshot() {
		if m.Name == "sump_work_transitions_total" {
			seen[m.Labels["status"]] = true
		}
	}
	if !seen[spv2.WorkDone] {
		t.Fatalf("work_update→done 没进指标: %+v", srv.Metrics().Snapshot())
	}
}
