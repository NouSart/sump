package tests

// 生产 A2A 队列与 work 生命周期（spec §6.8/§7.4/§7.5）：真实 recall 建 work 后走
// message_post/claim/ack/nack、work_cancel/work_renew，错误码与状态都由真库决定。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestA2AQueueAndLifecycleViaNative(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())
	payload := protocol.ContextPayload{
		Conversation: "conv-a2a",
		Boss:         "stub-boss",
		Messages:     []protocol.Message{{Role: "user", Content: "跑一个 A2A 任务"}},
	}
	caller.contextUntilReady(t, srv.WsURL(), payload)

	// 入队：to 是 reasoner 自己，才能被自己 claim。
	rp := caller.call(t, srv.WsURL(), "message_post", map[string]any{
		"work": "trace-test", "to": map[string]any{"kind": "agent", "id": "reasoner"},
		"method": "message", "body": "hello a2a",
	})
	if !rp.OK {
		t.Fatalf("message_post 失败: %s", rp.Error)
	}
	var posted struct {
		Message store.WorkMessage `msgpack:"message"`
	}
	if err := protocol.DecodeRaw(rp.Data, &posted); err != nil {
		t.Fatal(err)
	}
	if posted.Message.ID == "" || posted.Message.State != store.WorkMsgPending {
		t.Fatalf("入队结果不对: %+v", posted.Message)
	}

	// claim 到本人消息，ack 进 delivered。
	rp = caller.call(t, srv.WsURL(), "message_claim", map[string]any{"work": "trace-test"})
	if !rp.OK {
		t.Fatalf("message_claim 失败: %s", rp.Error)
	}
	var claimed struct {
		Message store.WorkMessage `msgpack:"message"`
	}
	if err := protocol.DecodeRaw(rp.Data, &claimed); err != nil {
		t.Fatal(err)
	}
	if claimed.Message.ID != posted.Message.ID || claimed.Message.State != store.WorkMsgClaimed {
		t.Fatalf("claim 结果不对: %+v", claimed.Message)
	}
	if rp = caller.call(t, srv.WsURL(), "message_ack", map[string]any{"id": claimed.Message.ID}); !rp.OK {
		t.Fatalf("message_ack 失败: %s", rp.Error)
	}
	if rp = caller.call(t, srv.WsURL(), "message_ack", map[string]any{"id": claimed.Message.ID}); rp.OK {
		t.Fatal("重复 ack 应 conflict")
	}

	// 别人的消息领不到：not_found，不是空壳。
	if rp = caller.call(t, srv.WsURL(), "message_post", map[string]any{
		"work": "trace-test", "to": map[string]any{"kind": "agent", "id": "other"},
		"body": "for other",
	}); !rp.OK {
		t.Fatalf("message_post 失败: %s", rp.Error)
	}
	if rp = caller.call(t, srv.WsURL(), "message_claim", map[string]any{"work": "trace-test"}); rp.OK {
		t.Fatal("领别人的消息应失败")
	} else if !strings.Contains(rp.Error, "not_found") {
		t.Fatalf("领空队列的错误应 not_found: %s", rp.Error)
	}

	// cancel：§6.5 只有 owner/runtime；普通服务身份必须 permission_denied。
	if rp = caller.call(t, srv.WsURL(), "work_cancel", map[string]any{"work": "trace-test", "reason": "用户停手"}); rp.OK {
		t.Fatal("非 owner/runtime 的 cancel 应被拒")
	} else if !strings.Contains(rp.Error, "permission_denied") {
		t.Fatalf("cancel 越权错误应 permission_denied: %s", rp.Error)
	}
	ctxC, cancelC := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelC()
	rc, err := spv2.Dial(ctxC, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	cancelCall, err := spv2.NewCall("trace-test", spv2.Ref{Kind: "agent", ID: "runtime", Authority: spv2.AuthorityRuntime}, spv2.ServiceRef("memory"), "work_cancel", map[string]any{"work": "trace-test", "reason": "用户停手"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := rc.Call(ctxC, cancelCall); err != nil {
		t.Fatalf("runtime cancel 失败: %v", err)
	}
	var got struct {
		Work spv2.Work `msgpack:"work"`
	}
	if rp = caller.call(t, srv.WsURL(), "work_get", map[string]any{"work": "trace-test"}); !rp.OK {
		t.Fatalf("work_get 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &got); err != nil {
		t.Fatal(err)
	}
	if got.Work.Status != spv2.WorkCancelled {
		t.Fatalf("cancel 后状态 = %q, want cancelled", got.Work.Status)
	}
	if _, err := rc.Call(ctxC, cancelCall); err == nil {
		t.Fatal("重复 cancel 应 conflict")
	} else if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("重复 cancel 应 conflict: %v", err)
	}
}

// §12.3：A2A 队列必须有 sump_queue_depth{state}，且随 claim/ack 真实变化。
func TestA2AQueueDepthMetric(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())
	payload := protocol.ContextPayload{
		Conversation: "conv-depth", Boss: "stub-boss",
		Messages: []protocol.Message{{Role: "user", Content: "队列深度"}},
	}
	caller.contextUntilReady(t, srv.WsURL(), payload)
	if rp := caller.call(t, srv.WsURL(), "message_post", map[string]any{
		"work": "trace-test", "to": map[string]any{"kind": "agent", "id": "reasoner"},
		"body": "depth",
	}); !rp.OK {
		t.Fatalf("message_post 失败: %s", rp.Error)
	}
	if got := queueDepth(srv, store.WorkMsgPending); got != 1 {
		t.Fatalf("post 后 pending queue_depth = %v, want 1", got)
	}
	claim := caller.call(t, srv.WsURL(), "message_claim", map[string]any{"work": "trace-test"})
	if !claim.OK {
		t.Fatalf("message_claim 失败: %s", claim.Error)
	}
	if got := queueDepth(srv, store.WorkMsgPending); got != 0 {
		t.Fatalf("claim 后 pending queue_depth = %v, want 0", got)
	}
	if got := queueDepth(srv, store.WorkMsgClaimed); got != 1 {
		t.Fatalf("claim 后 claimed queue_depth = %v, want 1", got)
	}
}

// queueDepth 读某 state 的 sump_queue_depth 快照。
func queueDepth(srv *server.Server, state string) float64 {
	for _, m := range srv.Metrics().Snapshot() {
		if m.Name == "sump_queue_depth" && m.Labels["state"] == state {
			return m.Value
		}
	}
	return -1
}
