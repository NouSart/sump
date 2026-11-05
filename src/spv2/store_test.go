package spv2

import (
	"context"
	"testing"
	"time"
)

func testWork() Work {
	now := time.Now()
	return Work{
		ID: "w1", Root: "w1", Class: ClassRequest,
		Owner: HumanRef("owner"), Agent: AgentRef("reasoner"), Entry: ServiceRef("frontend"),
		Status: WorkOpen, Lease: DefaultLease(now), CreatedAt: now.Format(time.RFC3339),
	}
}

func TestEnsureWorkIdempotent(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	w, err := s.EnsureWork(ctx, testWork())
	if err != nil {
		t.Fatal(err)
	}
	if w.Root != "w1" || w.Status != WorkOpen {
		t.Fatalf("work = %+v", w)
	}
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetWork(ctx, "w1"); err != nil {
		t.Fatal(err)
	}
}

func TestLeaseChargeRelease(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	rem, err := s.Charge(ctx, "w1", Charge{ToolCalls: 2, Tokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if rem.ToolCalls != 48 || rem.Tokens != 99900 {
		t.Fatalf("remaining = %+v", rem)
	}
	if err := s.Release(ctx, "w1", Charge{ToolCalls: 1}); err != nil {
		t.Fatal(err)
	}
	rem, _ = s.Remaining(ctx, "w1")
	if rem.ToolCalls != 49 {
		t.Fatalf("remaining = %+v", rem)
	}
	if _, err := s.Charge(ctx, "w1", Charge{Tokens: 1 << 30}); err == nil {
		t.Fatal("expected lease exhausted")
	}
}

func TestQueueClaimAckNack(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	id, err := s.PostMessage(ctx, QueuedMessage{WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"), Method: "message", Body: []byte("hi")})
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.ClaimMessage(ctx, "w1", time.Minute)
	if err != nil || m == nil || m.ID != id {
		t.Fatalf("claim = %+v, %v", m, err)
	}
	if err := s.AckMessage(ctx, id); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QueueDepth(ctx, "w1"); n != 0 {
		t.Fatalf("depth = %d", n)
	}
}

func TestSweepExpiresClaimed(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	id, _ := s.PostMessage(ctx, QueuedMessage{WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"), Method: "message"})
	// claim with tiny lease, then sweep in the future
	if _, err := s.ClaimMessage(ctx, "w1", time.Millisecond); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	res, err := s.Sweep(ctx, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if res.Retried != 1 || res.Stalled != 1 {
		t.Fatalf("sweep = %+v", res)
	}
	if m, _ := s.ClaimMessage(ctx, "w1", time.Minute); m == nil || m.ID != id {
		t.Fatalf("message not retried: %+v", m)
	}
}

func TestDoneRejectsPendingMessage(t *testing.T) {
	s := NewMemStore()
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(ctx, QueuedMessage{WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"), Method: "message"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", WorkDone); err == nil {
		t.Fatal("expected conflict")
	}
}
