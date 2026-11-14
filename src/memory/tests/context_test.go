package tests

// spec §7.2 context：reasoner 同步向 memory 要上下文；返回组装后的 messages + work 行。
// 这条路径替代 recall+resume 两跳（recall 保留兼容期，迁移完成后再删）。

import (
	"context"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestContextReturnsMessagesAndWork(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	req := protocol.ContextPayload{
		Conversation: "conv-context",
		Boss:         "stub-boss",
		TaskName:     "上下文任务",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "给我上下文"},
		},
	}
	rp := caller.call(t, srv.WsURL(), "context", req)
	if !rp.OK {
		t.Fatalf("context 失败: %s", rp.Error)
	}
	var got struct {
		Messages []protocol.Message `msgpack:"messages"`
		Work     spv2.Work          `msgpack:"work"`
		Task     protocol.Task      `msgpack:"task"`
	}
	if err := protocol.DecodeRaw(rp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Messages) == 0 {
		t.Fatal("context 返回空消息")
	}
	if got.Work.ID != "trace-test" || got.Work.Status != spv2.WorkRunning {
		t.Fatalf("context 返回的 work 不对: %+v", got.Work)
	}
	if got.Task.Name != "上下文任务" {
		t.Fatalf("context 返回的 task 不对: %+v", got.Task)
	}

	// 重复调用幂等：同一 work 行，不新建、不换 id。
	rp2 := caller.call(t, srv.WsURL(), "context", req)
	if !rp2.OK {
		t.Fatalf("第二次 context 失败: %s", rp2.Error)
	}
	var got2 struct {
		Work spv2.Work `msgpack:"work"`
	}
	if err := protocol.DecodeRaw(rp2.Data, &got2); err != nil {
		t.Fatal(err)
	}
	if got2.Work.ID != got.Work.ID || got2.Work.CreatedAt != got.Work.CreatedAt {
		t.Fatalf("重复 context 不应重建 work: %+v vs %+v", got2.Work, got.Work)
	}

	// 空消息必须 invalid_argument，不能静默返回空上下文。
	rp3 := caller.call(t, srv.WsURL(), "context", protocol.ContextPayload{Conversation: "c"})
	if rp3.OK {
		t.Fatal("空 messages 的 context 应失败")
	}
}

// spec §6.5：context 必须校验来源——work 已存在时只有它的 agent/owner/entry 能取；
// 终态 work 不能再要 context。
func TestContextAuthorityAndTerminal(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())
	req := protocol.ContextPayload{Conversation: "conv-auth", Boss: "stub-boss", Messages: []protocol.Message{{Role: "user", Content: "跑"}}}
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
	// 终态 work 再取 context：conflict。
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
