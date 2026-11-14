package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/spv2"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func testWork() spv2.Work {
	return spv2.Work{
		ID: "w1", Root: "w1", Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"), Entry: spv2.ServiceRef("frontend"),
		Goal: "测试", Status: spv2.WorkOpen,
	}
}

func TestWorkLedgerDefaultsAndIdempotent(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	w, err := s.EnsureWork(ctx, testWork())
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != spv2.WorkOpen || w.Lease.MaxToolCalls != 50 || w.Lease.MaxTokens != 100000 {
		t.Fatalf("默认值不对: %+v", w)
	}
	if w.CreatedAt == "" {
		t.Fatal("created_at 未填")
	}
	w2, err := s.EnsureWork(ctx, testWork())
	if err != nil {
		t.Fatal(err)
	}
	if w2.CreatedAt != w.CreatedAt {
		t.Fatalf("重复登记不应重建: %+v vs %+v", w, w2)
	}
}

func TestWorkStateMachineRejectsIllegalTransition(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkOpen); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("running→open 应 conflict: %v", err)
	}
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkDone); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkRunning); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("done→running 应 conflict: %v", err)
	}
}

func TestLeaseChargeReleaseAndStalled(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, testWork()); err != nil {
		t.Fatal(err)
	}
	rem, err := s.Charge(ctx, "w1", spv2.Charge{ToolCalls: 2})
	if err != nil {
		t.Fatal(err)
	}
	if rem.ToolCalls != 48 {
		t.Fatalf("remaining = %+v, want tool_calls 48", rem)
	}
	if _, err := s.Charge(ctx, "w1", spv2.Charge{ToolCalls: 9999}); !spv2.IsCode(err, spv2.CodeResourceExhausted) {
		t.Fatalf("超预算应 resource_exhausted: %v", err)
	}
	if err := s.Release(ctx, "w1", spv2.Charge{ToolCalls: 9999}); err != nil {
		t.Fatal(err)
	}
	rem, err = s.Remaining(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if rem.ToolCalls != 50 {
		t.Fatalf("Release 应 clamp 到上限，got %+v", rem)
	}
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkStalled); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Charge(ctx, "w1", spv2.Charge{ToolCalls: 1}); !spv2.IsCode(err, spv2.CodeStalled) {
		t.Fatalf("stalled work 扣租约应 stalled: %v", err)
	}
}

func TestLeaseDeadline(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	w := testWork()
	w.Lease.DeadlineAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.EnsureWork(ctx, w); err != nil {
		t.Fatal(err)
	}
	_, err := s.Charge(ctx, "w1", spv2.Charge{ToolCalls: 1})
	if !spv2.IsCode(err, spv2.CodeDeadlineExceeded) {
		t.Fatalf("过期 work 扣租约应 deadline_exceeded: %v", err)
	}
}

func TestSweepExpiredMarksStalledOnlyNonTerminal(t *testing.T) {
	s := openTestStore(t)
	ctx := context.Background()
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	mk := func(id, status, deadline string) {
		w := testWork()
		w.ID, w.Root, w.Status = id, id, status
		w.Lease.DeadlineAt = deadline
		if _, err := s.EnsureWork(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	mk("expired-running", spv2.WorkRunning, past)
	mk("expired-open", spv2.WorkOpen, past)
	mk("future-running", spv2.WorkRunning, future)
	mk("done-expired", spv2.WorkDone, past)

	n, err := s.SweepExpired(ctx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("sweep 数量 = %d, want 2", n)
	}
	for id, want := range map[string]string{
		"expired-running": spv2.WorkStalled,
		"expired-open":    spv2.WorkStalled,
		"future-running":  spv2.WorkRunning,
		"done-expired":    spv2.WorkDone,
	} {
		w, err := s.GetWork(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		if w.Status != want {
			t.Fatalf("%s status = %q, want %q", id, w.Status, want)
		}
	}
}
