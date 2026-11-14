package tests

import (
	"context"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// spec §6/§6.8：生产 memory 的 recall 必须把 work 登记进账本，并可通过内部 method
// 读 work 行、扣/还租约；越权调用要 permission_denied。
func TestWorkLedgerViaRecall(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	payload := protocol.ContextPayload{
		Conversation: "conv-work",
		Boss:         "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "把这轮跑起来"},
		},
	}
	caller.contextUntilReady(t, srv.WsURL(), payload)

	// work_get：work 行必须存在，且归属与 recall 帧一致。
	rp := caller.call(t, srv.WsURL(), "work_get", map[string]any{"work": "trace-test"})
	if !rp.OK {
		t.Fatalf("work_get 失败: %s", rp.Error)
	}
	var got struct {
		Work spv2.Work `msgpack:"work"`
	}
	if err := protocol.DecodeRaw(rp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Work.ID != "trace-test" || got.Work.Root != "trace-test" {
		t.Fatalf("work 行不对: %+v", got.Work)
	}
	if got.Work.Status != spv2.WorkRunning {
		t.Fatalf("work 状态 = %q, want running（recall 即开跑）", got.Work.Status)
	}
	if got.Work.Agent.ID != "reasoner" || got.Work.Entry.ID != "stub-boss" {
		t.Fatalf("work 归属不对: agent=%+v entry=%+v", got.Work.Agent, got.Work.Entry)
	}
	if got.Work.Lease.MaxToolCalls != 50 {
		t.Fatalf("默认租约没落库: %+v", got.Work.Lease)
	}

	// lease_charge：能从 50 扣到 49。
	rp = caller.call(t, srv.WsURL(), "lease_charge", map[string]any{
		"work": "trace-test", "delta": map[string]any{"tool_calls": 1},
	})
	if !rp.OK {
		t.Fatalf("lease_charge 失败: %s", rp.Error)
	}
	var charged struct {
		Remaining spv2.Remaining `msgpack:"remaining"`
	}
	if err := protocol.DecodeRaw(rp.Data, &charged); err != nil {
		t.Fatal(err)
	}
	if charged.Remaining.ToolCalls != 49 {
		t.Fatalf("remaining.tool_calls = %d, want 49", charged.Remaining.ToolCalls)
	}

	// lease_release：归还后回到上限。
	rp = caller.call(t, srv.WsURL(), "lease_release", map[string]any{
		"work": "trace-test", "delta": map[string]any{"tool_calls": 10},
	})
	if !rp.OK {
		t.Fatalf("lease_release 失败: %s", rp.Error)
	}
	rp = caller.call(t, srv.WsURL(), "work_get", map[string]any{"work": "trace-test"})
	if err := protocol.DecodeRaw(rp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Work.Lease.MaxToolCalls != 50 {
		t.Fatalf("Release 没 clamp 到上限: %+v", got.Work.Lease)
	}

	// work_update：按 §6.3 状态机从 running 推进到 done。
	rp = caller.call(t, srv.WsURL(), "work_update", map[string]any{"work": "trace-test", "status": spv2.WorkDone})
	if !rp.OK {
		t.Fatalf("work_update 失败: %s", rp.Error)
	}
	rp = caller.call(t, srv.WsURL(), "work_get", map[string]any{"work": "trace-test"})
	if err := protocol.DecodeRaw(rp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Work.Status != spv2.WorkDone {
		t.Fatalf("work 状态 = %q, want done", got.Work.Status)
	}
	// 终态不能再被推进回 running：非法转移必须 conflict。
	rp = caller.call(t, srv.WsURL(), "work_update", map[string]any{"work": "trace-test", "status": spv2.WorkRunning})
	if rp.OK {
		t.Fatal("done→running 应被拒绝")
	}

	// 越权：不是 owner/runtime/agent/entry，必须 permission_denied。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := spv2.NewCall("trace-test", spv2.ServiceRef("intruder"), spv2.ServiceRef("memory"), "lease_charge", map[string]any{
		"work": "trace-test", "delta": map[string]any{"tool_calls": 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, f); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("越权 lease_charge err = %v, want permission_denied", err)
	}
}

// 周期维护（spec §6.9 第 1 条）：deadline 已过且未终态的 work 由 memory 自己扫成 stalled，
// 记 sump_work_stalled_total{reason=deadline}；终态/未过期的一律不动。
func TestWorkSweepMarksExpiredStalled(t *testing.T) {
	sc := startStubCenter(t)
	var workStore *store.Store
	srv := startMemory(t, sc.URL(), 12, func(_ *server.Config, st *store.Store) { workStore = st })
	caller := startStubCaller(t, sc.URL())
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := workStore.EnsureWork(context.Background(), spv2.Work{
		ID: "w-expired", Root: "w-expired", Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("stub-boss"), Status: spv2.WorkRunning,
		Lease: spv2.Lease{DeadlineAt: past},
	}); err != nil {
		t.Fatal(err)
	}
	if rp := caller.call(t, srv.WsURL(), "work_sweep", map[string]any{}); !rp.OK {
		t.Fatalf("work_sweep 失败: %s", rp.Error)
	}
	w, err := workStore.GetWork(context.Background(), "w-expired")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != spv2.WorkStalled {
		t.Fatalf("过期 work 状态 = %q, want stalled", w.Status)
	}
	found := false
	for _, m := range srv.Metrics().Snapshot() {
		if m.Name == "sump_work_stalled_total" && m.Labels["reason"] == "deadline" && m.Value == 1 {
			found = true
		}
	}
	if !found {
		t.Fatalf("缺少 sump_work_stalled_total{reason=deadline}=1: %+v", srv.Metrics().Snapshot())
	}
}
