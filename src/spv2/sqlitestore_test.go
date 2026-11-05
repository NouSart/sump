package spv2

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func newSQLiteTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := NewSQLiteStore(filepath.Join(t.TempDir(), "spv2.db"))
	if err != nil {
		t.Fatalf("NewSQLiteStore: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	return s
}

func sqliteTestWork(id string) Work {
	return Work{
		ID: id, Root: id, Class: ClassRequest,
		Owner: HumanRef("owner"), Agent: AgentRef("reasoner"), Entry: ServiceRef("frontend"),
		Status: WorkOpen, Lease: DefaultLease(time.Now()),
	}
}

func TestSQLiteEnsureWorkIdempotent(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()

	w := sqliteTestWork("w1")
	w.Class = ""
	w.Status = ""
	w.Root = ""
	w.Goal = "first"
	w.Participants = []Participant{{Actor: AgentRef("worker-1"), Role: "member", Status: "active"}}
	got, err := s.EnsureWork(ctx, w)
	if err != nil {
		t.Fatal(err)
	}
	if got.Root != "w1" {
		t.Fatalf("root 默认值 = %q", got.Root)
	}
	if got.Class != ClassRequest {
		t.Fatalf("class 默认值 = %q", got.Class)
	}
	if got.Status != WorkOpen {
		t.Fatalf("status 默认值 = %q", got.Status)
	}
	if got.CreatedAt == "" || got.UpdatedAt == "" {
		t.Fatalf("时间戳未填: %+v", got)
	}
	if got.Lease.MaxToolCalls != 50 || got.Lease.MaxTokens != 100000 || got.Lease.DeadlineAt == "" {
		t.Fatalf("lease 默认值 = %+v", got.Lease)
	}
	if len(got.Participants) != 1 || got.Participants[0].Actor.ID != "worker-1" {
		t.Fatalf("participants = %+v", got.Participants)
	}

	again := sqliteTestWork("w1")
	again.Goal = "second"
	again.Status = WorkRunning
	got2, err := s.EnsureWork(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Goal != "first" || got2.Status != WorkOpen {
		t.Fatalf("重复 EnsureWork 覆盖了已有 work: %+v", got2)
	}
	if len(got2.Participants) != 1 {
		t.Fatalf("participants = %+v", got2.Participants)
	}

	if _, err := s.EnsureWork(ctx, Work{}); err == nil {
		t.Fatal("非法 work 应当报错")
	}
}

func TestSQLitePersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "spv2.db")
	ctx := context.Background()

	s1, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s1.EnsureWork(ctx, sqliteTestWork("w1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.Charge(ctx, "w1", Charge{ToolCalls: 7, Tokens: 5000}); err != nil {
		t.Fatal(err)
	}
	if _, err := s1.PostMessage(ctx, QueuedMessage{
		WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"), Method: "message", Body: []byte("hi"),
	}); err != nil {
		t.Fatal(err)
	}
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewSQLiteStore(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s2.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	rem, err := s2.Remaining(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if rem.ToolCalls != 43 || rem.Tokens != 95000 {
		t.Fatalf("remaining = %+v", rem)
	}
	if n, err := s2.QueueDepth(ctx, "w1"); err != nil || n != 1 {
		t.Fatalf("depth = %d, %v", n, err)
	}
	w, err := s2.GetWork(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != WorkOpen || w.Lease.MaxToolCalls != 50 {
		t.Fatalf("work = %+v", w)
	}
}

func TestSQLiteLeaseChargeRelease(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, sqliteTestWork("w1")); err != nil {
		t.Fatal(err)
	}

	rem, err := s.Charge(ctx, "w1", Charge{ToolCalls: 2, Tokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if rem.ToolCalls != 48 || rem.Tokens != 99900 {
		t.Fatalf("remaining = %+v", rem)
	}
	rem, err = s.Remaining(ctx, "w1")
	if err != nil || rem.ToolCalls != 48 || rem.Tokens != 99900 {
		t.Fatalf("remaining = %+v, %v", rem, err)
	}

	if _, err := s.Charge(ctx, "w1", Charge{ToolCalls: 1 << 20}); !errors.Is(err, ErrLeaseExhausted) {
		t.Fatalf("err = %v，期望 ErrLeaseExhausted", err)
	}
	if rem, _ := s.Remaining(ctx, "w1"); rem.ToolCalls != 48 || rem.Tokens != 99900 {
		t.Fatalf("预算不足时发生了部分扣减: %+v", rem)
	}

	if err := s.Release(ctx, "w1", Charge{ToolCalls: 1}); err != nil {
		t.Fatal(err)
	}
	if rem, _ := s.Remaining(ctx, "w1"); rem.ToolCalls != 49 {
		t.Fatalf("remaining = %+v", rem)
	}
	if err := s.Release(ctx, "w1", Charge{ToolCalls: 1000, Tokens: 1 << 20}); err != nil {
		t.Fatal(err)
	}
	rem, _ = s.Remaining(ctx, "w1")
	if rem.ToolCalls != 50 || rem.Tokens != 100000 {
		t.Fatalf("归还未按上限封顶: %+v", rem)
	}

	expired := sqliteTestWork("w2")
	expired.Lease = DefaultLease(time.Now())
	expired.Lease.DeadlineAt = time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.EnsureWork(ctx, expired); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Charge(ctx, "w2", Charge{ToolCalls: 1}); !IsCode(err, CodeDeadlineExceeded) {
		t.Fatalf("err = %v，期望 deadline_exceeded", err)
	}

	if _, err := s.Charge(ctx, "missing", Charge{ToolCalls: 1}); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
	if _, err := s.Remaining(ctx, "missing"); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
	if err := s.Release(ctx, "missing", Charge{ToolCalls: 1}); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
	if _, err := s.QueueDepth(ctx, "missing"); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
	if _, err := s.GetWork(ctx, "missing"); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
}

func TestSQLiteLeaseChargeConcurrent(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, sqliteTestWork("w1")); err != nil {
		t.Fatal(err)
	}

	var ok, exhausted int32
	var wg sync.WaitGroup
	for i := 0; i < 60; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := s.Charge(ctx, "w1", Charge{ToolCalls: 1})
			switch {
			case err == nil:
				atomic.AddInt32(&ok, 1)
			case errors.Is(err, ErrLeaseExhausted):
				atomic.AddInt32(&exhausted, 1)
			default:
				t.Errorf("意外的扣减错误: %v", err)
			}
		}()
	}
	wg.Wait()
	if ok != 50 || exhausted != 10 {
		t.Fatalf("ok=%d exhausted=%d", ok, exhausted)
	}
	if rem, _ := s.Remaining(ctx, "w1"); rem.ToolCalls != 0 {
		t.Fatalf("remaining = %+v", rem)
	}
}

func TestSQLiteQueueClaimAckNack(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, sqliteTestWork("w1")); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	first, err := s.PostMessage(ctx, QueuedMessage{
		ID: "m1", WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"),
		Method: "message", Body: []byte("first"), CreatedAt: now.Add(-2 * time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PostMessage(ctx, QueuedMessage{
		ID: "m2", WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"),
		Method: "message", Body: []byte("second"), CreatedAt: now.Add(-time.Second),
	})
	if err != nil {
		t.Fatal(err)
	}
	if n, err := s.QueueDepth(ctx, "w1"); err != nil || n != 2 {
		t.Fatalf("depth = %d, %v", n, err)
	}

	m, err := s.ClaimMessage(ctx, "w1", time.Minute)
	if err != nil || m == nil {
		t.Fatalf("claim = %+v, %v", m, err)
	}
	if m.ID != first || m.State != MessageClaimed || m.Attempts != 1 || string(m.Body) != "first" {
		t.Fatalf("claim = %+v", m)
	}
	if !m.LeaseUntil.After(time.Now()) {
		t.Fatalf("lease_until = %v", m.LeaseUntil)
	}
	if err := s.AckMessage(ctx, first); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QueueDepth(ctx, "w1"); n != 1 {
		t.Fatalf("depth = %d", n)
	}

	m, err = s.ClaimMessage(ctx, "w1", time.Minute)
	if err != nil || m == nil || m.ID != second || m.Attempts != 1 {
		t.Fatalf("claim = %+v, %v", m, err)
	}
	if err := s.NackMessage(ctx, second, "boom"); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ClaimMessage(ctx, "w1", time.Minute)
	if m == nil || m.Attempts != 2 || m.LastError != "boom" {
		t.Fatalf("第二次 claim = %+v", m)
	}
	if err := s.NackMessage(ctx, second, "boom"); err != nil {
		t.Fatal(err)
	}
	m, _ = s.ClaimMessage(ctx, "w1", time.Minute)
	if m == nil || m.Attempts != 3 {
		t.Fatalf("第三次 claim = %+v", m)
	}
	if err := s.NackMessage(ctx, second, "final"); err != nil {
		t.Fatal(err)
	}
	if n, _ := s.QueueDepth(ctx, "w1"); n != 0 {
		t.Fatalf("depth = %d", n)
	}
	if m, err := s.ClaimMessage(ctx, "w1", time.Minute); err != nil || m != nil {
		t.Fatalf("claim = %+v, %v", m, err)
	}

	if err := s.AckMessage(ctx, "missing"); !IsCode(err, CodeNotFound) {
		t.Fatalf("err = %v，期望 not_found", err)
	}
	if err := s.NackMessage(ctx, "missing", "x"); !IsCode(err, CodeNotFound) {
		t.Fatalf("err = %v，期望 not_found", err)
	}
	if _, err := s.PostMessage(ctx, QueuedMessage{WorkID: "w1"}); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("err = %v，期望 invalid_argument", err)
	}
	if _, err := s.PostMessage(ctx, QueuedMessage{WorkID: "missing", Method: "message"}); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
}

func TestSQLiteDoneRejectsPendingAndChildren(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()

	if _, err := s.EnsureWork(ctx, sqliteTestWork("w1")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", WorkRunning); err != nil {
		t.Fatal(err)
	}
	id, err := s.PostMessage(ctx, QueuedMessage{
		WorkID: "w1", From: AgentRef("A"), To: AgentRef("B"), Method: "message",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", WorkDone); !IsCode(err, CodeConflict) {
		t.Fatalf("err = %v，期望 conflict", err)
	}
	if err := s.AckMessage(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", WorkDone); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetWork(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != WorkDone || got.ClosedAt == "" {
		t.Fatalf("work = %+v", got)
	}

	if _, err := s.EnsureWork(ctx, sqliteTestWork("w2")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w2", WorkDone); !IsCode(err, CodeConflict) {
		t.Fatalf("open->done 应当 conflict，err = %v", err)
	}

	parent := sqliteTestWork("p1")
	if _, err := s.EnsureWork(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "p1", WorkRunning); err != nil {
		t.Fatal(err)
	}
	child := sqliteTestWork("c1")
	child.Parent = "p1"
	child.Root = "p1"
	if _, err := s.EnsureWork(ctx, child); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "p1", WorkDone); !IsCode(err, CodeConflict) {
		t.Fatalf("未终态 child 应当 conflict，err = %v", err)
	}
	if err := s.UpdateStatus(ctx, "c1", WorkCancelled); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "p1", WorkDone); err != nil {
		t.Fatal(err)
	}

	if err := s.UpdateStatus(ctx, "missing", WorkRunning); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
}

func TestSQLiteParticipantAndRenew(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()
	if _, err := s.EnsureWork(ctx, sqliteTestWork("w1")); err != nil {
		t.Fatal(err)
	}

	if err := s.AddParticipant(ctx, "missing", Participant{Actor: AgentRef("x")}); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
	if err := s.AddParticipant(ctx, "w1", Participant{Actor: AgentRef("worker-1")}); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetWork(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if len(w.Participants) != 1 || w.Participants[0].Role != "member" || w.Participants[0].Status != "active" {
		t.Fatalf("participants = %+v", w.Participants)
	}
	if err := s.AddParticipant(ctx, "w1", Participant{Actor: AgentRef("worker-1"), Role: "reviewer", Status: "idle"}); err != nil {
		t.Fatal(err)
	}
	w, _ = s.GetWork(ctx, "w1")
	if len(w.Participants) != 1 || w.Participants[0].Role != "reviewer" || w.Participants[0].Status != "idle" {
		t.Fatalf("participants = %+v", w.Participants)
	}

	if _, err := s.Charge(ctx, "w1", Charge{ToolCalls: 10}); err != nil {
		t.Fatal(err)
	}
	l := DefaultLease(time.Now())
	l.MaxToolCalls = 60
	l.DeadlineAt = time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	renewed, err := s.Renew(ctx, "w1", l)
	if err != nil {
		t.Fatal(err)
	}
	if renewed.Lease.MaxToolCalls != 60 || renewed.Lease.DeadlineAt != l.DeadlineAt {
		t.Fatalf("lease = %+v", renewed.Lease)
	}
	if rem, _ := s.Remaining(ctx, "w1"); rem.ToolCalls != 50 || rem.DeadlineAt != l.DeadlineAt {
		t.Fatalf("remaining = %+v", rem)
	}
	if _, err := s.Renew(ctx, "w1", Lease{MaxToolCalls: 45}); err != nil {
		t.Fatal(err)
	}
	if rem, _ := s.Remaining(ctx, "w1"); rem.ToolCalls != 50 {
		t.Fatalf("缩小上限不应减少余额: %+v", rem)
	}
	if _, err := s.Renew(ctx, "w1", Lease{DeadlineAt: "not-a-time"}); !IsCode(err, CodeInvalidArgument) {
		t.Fatalf("err = %v，期望 invalid_argument", err)
	}

	if err := s.UpdateStatus(ctx, "w1", WorkRunning); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w1", WorkDone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Renew(ctx, "w1", l); !IsCode(err, CodeConflict) {
		t.Fatalf("终态续租 err = %v，期望 conflict", err)
	}
	if _, err := s.Renew(ctx, "missing", l); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("err = %v，期望 ErrWorkNotFound", err)
	}
}

func TestSQLiteSweep(t *testing.T) {
	s := newSQLiteTestStore(t)
	ctx := context.Background()
	now := time.Now()

	expiredWork := sqliteTestWork("w-expired")
	expiredWork.Status = WorkRunning
	expiredWork.Lease = DefaultLease(now)
	expiredWork.Lease.DeadlineAt = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.EnsureWork(ctx, expiredWork); err != nil {
		t.Fatal(err)
	}

	doneWork := sqliteTestWork("w-done")
	doneWork.Status = WorkRunning
	doneWork.Lease = DefaultLease(now)
	doneWork.Lease.DeadlineAt = now.Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := s.EnsureWork(ctx, doneWork); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateStatus(ctx, "w-done", WorkDone); err != nil {
		t.Fatal(err)
	}

	if _, err := s.EnsureWork(ctx, sqliteTestWork("w-retry")); err != nil {
		t.Fatal(err)
	}
	retryID, err := s.PostMessage(ctx, QueuedMessage{
		WorkID: "w-retry", From: AgentRef("A"), To: AgentRef("B"), Method: "message",
		CreatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.ClaimMessage(ctx, "w-retry", -time.Minute); err != nil || m == nil || m.Attempts != 1 {
		t.Fatalf("claim = %+v, %v", m, err)
	}

	if _, err := s.EnsureWork(ctx, sqliteTestWork("w-fail")); err != nil {
		t.Fatal(err)
	}
	failID, err := s.PostMessage(ctx, QueuedMessage{
		WorkID: "w-fail", From: AgentRef("A"), To: AgentRef("B"), Method: "message",
	})
	if err != nil {
		t.Fatal(err)
	}
	if m, err := s.ClaimMessage(ctx, "w-fail", time.Minute); err != nil || m == nil {
		t.Fatalf("claim = %+v, %v", m, err)
	}
	if err := s.NackMessage(ctx, failID, "boom"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.ClaimMessage(ctx, "w-fail", time.Minute); m == nil {
		t.Fatal("期望第二次 claim")
	}
	if err := s.NackMessage(ctx, failID, "boom"); err != nil {
		t.Fatal(err)
	}
	if m, _ := s.ClaimMessage(ctx, "w-fail", -time.Minute); m == nil || m.Attempts != 3 {
		t.Fatalf("第三次 claim = %+v", m)
	}

	if _, err := s.EnsureWork(ctx, sqliteTestWork("w-old")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostMessage(ctx, QueuedMessage{
		WorkID: "w-old", From: AgentRef("A"), To: AgentRef("B"), Method: "message",
		CreatedAt: now.Add(-25 * time.Hour),
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := s.EnsureWork(ctx, sqliteTestWork("w-fresh")); err != nil {
		t.Fatal(err)
	}
	freshID, err := s.PostMessage(ctx, QueuedMessage{
		WorkID: "w-fresh", From: AgentRef("A"), To: AgentRef("B"), Method: "message",
		CreatedAt: now.Add(-time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := s.Sweep(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if res.Stalled != 1 || res.Retried != 2 || res.Expired != 1 {
		t.Fatalf("sweep = %+v", res)
	}
	if w, _ := s.GetWork(ctx, "w-expired"); w.Status != WorkStalled {
		t.Fatalf("status = %q", w.Status)
	}
	if w, _ := s.GetWork(ctx, "w-done"); w.Status != WorkDone {
		t.Fatalf("终态 work 被改动: %q", w.Status)
	}

	m, err := s.ClaimMessage(ctx, "w-retry", time.Minute)
	if err != nil || m == nil || m.ID != retryID || m.Attempts != 2 {
		t.Fatalf("重试后的 claim = %+v, %v", m, err)
	}
	if m, _ := s.ClaimMessage(ctx, "w-fail", time.Minute); m != nil {
		t.Fatalf("failed 消息不应再被领取: %+v", m)
	}
	if m, _ := s.ClaimMessage(ctx, "w-old", time.Minute); m != nil {
		t.Fatalf("expired 消息不应再被领取: %+v", m)
	}
	if m, _ := s.ClaimMessage(ctx, "w-fresh", time.Minute); m == nil || m.ID != freshID {
		t.Fatalf("fresh 消息应当可领取: %+v", m)
	}
}
