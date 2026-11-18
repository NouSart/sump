package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/spv2"
)

func openA2AStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func seedWork(t *testing.T, s *Store, id string) {
	t.Helper()
	if _, err := s.EnsureWork(context.Background(), spv2.Work{
		ID: id, Root: id, Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"), Entry: spv2.ServiceRef("frontend"),
		Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestA2APostClaimAck(t *testing.T) {
	s := openA2AStore(t)
	ctx := context.Background()
	seedWork(t, s, "w1")
	m, err := s.PostWorkMessage(ctx, "w1", spv2.AgentRef("a"), spv2.AgentRef("b"), "message", "hello")
	if err != nil {
		t.Fatal(err)
	}
	if m.State != WorkMsgPending || m.BodyRef == 0 {
		t.Fatalf("入队消息不对: %+v", m)
	}
	// 目标不是本人：领不到（不能把别人的消息偷走）。
	if _, err := s.ClaimWorkMessage(ctx, "w1", "agent", "c", time.Minute); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("领别人的消息应 not_found: %v", err)
	}
	got, err := s.ClaimWorkMessage(ctx, "w1", "agent", "b", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if got.State != WorkMsgClaimed || got.Attempts != 1 || got.LeaseUntil == "" {
		t.Fatalf("领取后的消息不对: %+v", got)
	}
	if got.Body != "hello" {
		t.Fatalf("claim 回执应带正文（只存一份的读取视图）: %+v", got)
	}
	if err := s.AckWorkMessage(ctx, got.ID); err != nil {
		t.Fatal(err)
	}
	acked, err := s.GetWorkMessage(ctx, got.ID)
	if err != nil {
		t.Fatal(err)
	}
	if acked.State != WorkMsgDelivered {
		t.Fatalf("ack 后状态 = %q, want delivered", acked.State)
	}
	if err := s.AckWorkMessage(ctx, got.ID); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("重复 ack 应 conflict: %v", err)
	}
}

func TestA2ANackRetriesThenDeadLetter(t *testing.T) {
	s := openA2AStore(t)
	ctx := context.Background()
	seedWork(t, s, "w1")
	m, err := s.PostWorkMessage(ctx, "w1", spv2.AgentRef("a"), spv2.AgentRef("b"), "message", "x")
	if err != nil {
		t.Fatal(err)
	}
	// max_attempts=3：前两次失败重排，第 3 次失败进 dead-letter。
	for attempt := 1; attempt <= 2; attempt++ {
		if _, err := s.ClaimWorkMessage(ctx, "w1", "agent", "b", time.Minute); err != nil {
			t.Fatalf("第 %d 次 claim 失败: %v", attempt, err)
		}
		retried, err := s.NackWorkMessage(ctx, m.ID, "boom", 3)
		if err != nil {
			t.Fatal(err)
		}
		if !retried {
			t.Fatalf("第 %d 次 nack 应重新入队", attempt)
		}
	}
	if _, err := s.ClaimWorkMessage(ctx, "w1", "agent", "b", time.Minute); err != nil {
		t.Fatal(err)
	}
	retried, err := s.NackWorkMessage(ctx, m.ID, "boom", 3)
	if err != nil {
		t.Fatal(err)
	}
	if retried {
		t.Fatal("超过重试上限应进 failed（dead-letter）")
	}
	dead, err := s.GetWorkMessage(ctx, m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if dead.State != WorkMsgFailed || dead.LastError != "boom" {
		t.Fatalf("dead-letter 状态不对: %+v", dead)
	}
}

func TestA2APostOnTerminalWorkConflicts(t *testing.T) {
	s := openA2AStore(t)
	ctx := context.Background()
	seedWork(t, s, "w1")
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkDone); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PostWorkMessage(ctx, "w1", spv2.AgentRef("a"), spv2.AgentRef("b"), "message", "x"); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("终态 work 入队应 conflict: %v", err)
	}
}

func TestRenewRefreshesLeaseAndRevivesStalled(t *testing.T) {
	s := openA2AStore(t)
	ctx := context.Background()
	seedWork(t, s, "w1")
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkStalled); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	if err := s.Renew(ctx, "w1", spv2.Lease{DeadlineAt: future, MaxToolCalls: 7}); err != nil {
		t.Fatal(err)
	}
	w, err := s.GetWork(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != spv2.WorkRunning || w.Lease.MaxToolCalls != 7 || w.Lease.DeadlineAt != future {
		t.Fatalf("renew 后 work 不对: %+v", w)
	}
	rem, err := s.Remaining(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if rem.ToolCalls != 7 {
		t.Fatalf("renew 应刷新余额，got %+v", rem)
	}
	// 不得把 deadline 改到过去。
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if err := s.Renew(ctx, "w1", spv2.Lease{DeadlineAt: past}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("renew 到过去应 invalid_argument: %v", err)
	}
	// 终态不能续租。
	if err := s.UpdateStatus(ctx, "w1", spv2.WorkCancelled); err != nil {
		t.Fatal(err)
	}
	if err := s.Renew(ctx, "w1", spv2.Lease{DeadlineAt: future}); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("终态 renew 应 conflict: %v", err)
	}
}
