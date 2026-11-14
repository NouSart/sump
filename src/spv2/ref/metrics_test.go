package ref_test

import (
	"testing"

	"github.com/NouSart/sump/spv2"
	"github.com/NouSart/sump/spv2/ref"
)

// spec §12.3：参考实现也要埋 work/queue/lease/A2A 的最小指标，不能只写在文档里。
func TestMemoryEmitsSpecMetrics(t *testing.T) {
	c := newChain(t)
	ctx, cancel := testContext(t)
	defer cancel()
	mem := dial(t, c.memoryURL)
	workID := spv2.NewID()
	from := spv2.AgentRef("reasoner").WithAuthority(spv2.AuthorityRuntime)

	// 1) context 建 work：sump_work_active{status=open} 必须出现。
	fc, err := spv2.NewCall(workID, spv2.AgentRef("reasoner"), spv2.ServiceRef("memory"), "context", ref.ContextRequest{
		Owner: spv2.HumanRef("owner"),
	})
	if err != nil {
		t.Fatal(err)
	}
	var cresp ref.ContextResponse
	if err := mem.CallDecode(ctx, fc, &cresp); err != nil {
		t.Fatalf("context: %v", err)
	}

	// context 刚建完 work 就必须有 open 的活跃计数（不能等 cancel 才第一次出现）。
	foundOpen := false
	for _, m := range c.memory.Server().Metrics().Snapshot() {
		if m.Name == "sump_work_active" && m.Labels["status"] == "open" && m.Value >= 1 {
			foundOpen = true
		}
	}
	if !foundOpen {
		t.Fatal("context 建 work 之后没有 sump_work_active{status=open}")
	}

	// 2) 超预算扣租约：lease_exhausted_total。
	fl, err := spv2.NewCall(workID, spv2.AgentRef("reasoner"), spv2.ServiceRef("memory"), "lease_charge", ref.LeaseChargeRequest{
		Delta: spv2.Charge{ToolCalls: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mem.Call(ctx, fl); !spv2.IsCode(err, spv2.CodeResourceExhausted) {
		t.Fatalf("lease_charge err = %v, want resource_exhausted", err)
	}

	// 3) A2A post → claim → ack：三个状态都要计数。
	fp, err := spv2.NewCall(workID, spv2.AgentRef("a"), spv2.ServiceRef("memory"), "message_post", ref.MessagePostRequest{
		Work: workID, From: spv2.AgentRef("a"), To: spv2.AgentRef("b"), Method: "message",
	})
	if err != nil {
		t.Fatal(err)
	}
	var presp ref.MessagePostResponse
	if err := mem.CallDecode(ctx, fp, &presp); err != nil {
		t.Fatalf("message_post: %v", err)
	}
	fcl, err := spv2.NewCall(workID, spv2.AgentRef("b"), spv2.ServiceRef("memory"), "message_claim", ref.MessageClaimRequest{Work: workID})
	if err != nil {
		t.Fatal(err)
	}
	var clresp ref.MessageClaimResponse
	if err := mem.CallDecode(ctx, fcl, &clresp); err != nil {
		t.Fatalf("message_claim: %v", err)
	}
	if clresp.Message == nil {
		t.Fatal("claim 没有拿到消息")
	}
	fa, err := spv2.NewCall(workID, spv2.AgentRef("b"), spv2.ServiceRef("memory"), "message_ack", ref.MessageAckRequest{ID: clresp.Message.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CallDecode(ctx, fa, &ref.MessageAckResponse{}); err != nil {
		t.Fatalf("message_ack: %v", err)
	}

	// 4) cancel：work_active 的状态必须从 open 迁到 cancelled。
	fcan, err := spv2.NewCall(workID, from, spv2.ServiceRef("memory"), "cancel", ref.CancelRequest{Work: workID})
	if err != nil {
		t.Fatal(err)
	}
	if err := mem.CallDecode(ctx, fcan, &ref.CancelResponse{}); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	ms := c.memory.Server().Metrics().Snapshot()
	counter := func(name, label, value string) float64 {
		for _, m := range ms {
			if m.Name == name && m.Labels[label] == value {
				return m.Value
			}
		}
		return 0
	}
	gauge := func(name, label, value string) float64 {
		for _, m := range ms {
			if m.Name == name && m.Labels[label] == value {
				return m.Value
			}
		}
		return 0
	}
	if got := gauge("sump_work_active", "status", "open"); got != 0 {
		t.Fatalf("cancel 之后 sump_work_active{status=open} = %v, want 0", got)
	}
	if got := gauge("sump_work_active", "status", "cancelled"); got < 1 {
		t.Fatalf("sump_work_active{status=cancelled} = %v, want >=1", got)
	}
	if got := counter("sump_lease_exhausted_total", "", ""); got < 1 {
		t.Fatalf("sump_lease_exhausted_total = %v, want >=1", got)
	}
	for _, state := range []string{spv2.MessagePending, spv2.MessageClaimed, spv2.MessageDelivered} {
		if got := counter("sump_a2a_messages_total", "state", state); got < 1 {
			t.Fatalf("sump_a2a_messages_total{state=%s} = %v, want >=1", state, got)
		}
	}
}
