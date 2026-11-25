package tests

// context/2 的公开入口（R-026 P2c）：work_open 登记 → store 产生触发条目 → facade 的
// context/2 的公开入口：返回 session 基座并注入 experience；请求只按 work + trigger 组装。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/facade/compose"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestContextReturnsBaseAndExperience(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	opened := caller.openWork(t, srv.WsURL(), "w-ctx", "conv-context", "上下文任务")
	caller.storeV2(t, srv.WsURL(), "w-ctx", "conv-context", opened.TaskID, "user", "上一轮", "reasoner:prev")
	current := caller.storeV2(t, srv.WsURL(), "w-ctx", "conv-context", opened.TaskID, "user", "给我上下文", "reasoner:cur")
	if rp := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{
		Kind: "偏好", Content: "用户喜欢简洁", Priority: 1,
	}); !rp.OK {
		t.Fatalf("remember 失败: %s", rp.Error)
	}

	got := caller.contextFor(t, srv.WsURL(), "w-ctx", protocol.Trigger{Kind: "user", Item: current.ItemID})
	if got.Task.ID != opened.TaskID || got.Task.Name != "上下文任务" {
		t.Fatalf("context 返回的 task 不对: %+v", got.Task)
	}
	if got.Work.ID != "w-ctx" || got.Work.Status != spv2.WorkRunning || got.Work.Agent != "reasoner" {
		t.Fatalf("context 返回的 work 不对: %+v", got.Work)
	}
	if got.TriggerState != "assembled" || got.SnapshotID == "" {
		t.Fatalf("首次组装状态不对: %+v", got)
	}
	var joined string
	for _, m := range got.Messages {
		joined += m.Content + "\n"
	}
	if !strings.Contains(joined, "上一轮") || !strings.Contains(joined, "给我上下文") {
		t.Fatalf("基座应包含全部库内历史: %+v", got.Messages)
	}
	block := memoryBlockText(got.Messages)
	if !strings.HasPrefix(block, compose.MemoryPrefix) || !strings.Contains(block, "用户喜欢简洁") {
		t.Fatalf("experience 注入不符: %q", block)
	}

	// assembled：同一触发复用同一基座快照；experience 每次现注入。
	second := caller.contextFor(t, srv.WsURL(), "w-ctx", protocol.Trigger{Kind: "user", Item: current.ItemID})
	if second.SnapshotID != got.SnapshotID {
		t.Fatalf("assembled 应复用快照: %q vs %q", second.SnapshotID, got.SnapshotID)
	}

	// 空 work / 缺触发必须 invalid_argument，不能静默返回空上下文。
	if rp := caller.call(t, srv.WsURL(), "context", protocol.ContextRequest{}); rp.OK {
		t.Fatal("空 context 请求应失败")
	} else if !strings.Contains(rp.Error, "work 必填") {
		t.Fatalf("错误应说明 work 必填: %s", rp.Error)
	}
	if rp := caller.call(t, srv.WsURL(), "context", protocol.ContextRequest{Work: "w-ctx"}); rp.OK {
		t.Fatal("缺 trigger 的 context 应失败")
	}
}

// TestContextAuthorityAndTerminal：work 已存在时只有它的 agent/owner/entry 能取；
// 终态 work 不能再要 context。
func TestContextAuthorityAndTerminal(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	opened := caller.openWork(t, srv.WsURL(), "trace-test", "conv-auth", "权限任务")
	msg := caller.storeV2(t, srv.WsURL(), "trace-test", "conv-auth", opened.TaskID, "user", "跑", "reasoner:a1")
	req := protocol.ContextRequest{
		Work: "trace-test", Trigger: protocol.Trigger{Kind: "user", Item: msg.ItemID},
	}
	if rp := caller.call(t, srv.WsURL(), "context", req); !rp.OK {
		t.Fatalf("首次 context 失败: %s", rp.Error)
	}

	// 另一个服务来取同一条 work：permission_denied。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	other, err := spv2.NewCall("trace-test", spv2.ServiceRef("intruder"), spv2.ServiceRef("memory"), "context", req)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, other); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("越权 context 应 permission_denied: %v", err)
	}

	// 普通服务 cancel 被拒；runtime 可把 work 置终态。
	if rp := caller.call(t, srv.WsURL(), "work_cancel", map[string]any{"work": "trace-test"}); rp.OK {
		t.Fatal("普通服务 cancel 应被拒")
	}
	rc, err := spv2.Dial(ctx, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	cancelCall, err := spv2.NewCall("trace-test", spv2.Ref{Kind: "agent", ID: "runtime", Authority: spv2.AuthorityRuntime}, spv2.ServiceRef("memory"), "work_cancel", map[string]any{"work": "trace-test"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Call(ctx, cancelCall); err != nil {
		t.Fatalf("runtime cancel 失败: %v", err)
	}
	if rp := caller.call(t, srv.WsURL(), "context", req); rp.OK {
		t.Fatal("终态 work 的 context 应 conflict")
	}
}
