package server

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// 同包单测：ensureContextWork 与 authorizeWork 的守卫必须在本包被守住，
// 不能只靠 tests 包（异变按文件定位到 ./server 跑）。
func newWorkTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Server{cfg: Config{Store: st}}, st
}

func TestEnsureRecallWorkRegistersRow(t *testing.T) {
	s, st := newWorkTestServer(t)
	ctx := context.Background()
	meta := callMeta{from: "reasoner", trace: "w1"}
	if err := s.ensureContextWork(ctx, meta, protocol.TaskContext{Boss: "frontend"}, "目标"); err != nil {
		t.Fatal(err)
	}
	w, err := st.GetWork(ctx, "w1")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != spv2.WorkRunning || w.Agent.ID != "reasoner" || w.Entry.ID != "frontend" || w.Goal != "目标" {
		t.Fatalf("work 行不对: %+v", w)
	}
	// 缺 frame.work 时必须 fail-closed，而不是登记一条空 ID。
	if err := s.ensureContextWork(ctx, callMeta{from: "reasoner"}, protocol.TaskContext{}, ""); err == nil {
		t.Fatal("缺 work id 应报错")
	}
}

func TestAuthorizeWorkMatrix(t *testing.T) {
	w := spv2.Work{ID: "w1", Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"), Entry: spv2.ServiceRef("frontend")}
	for _, tc := range []struct {
		name string
		from spv2.Ref
		ok   bool
	}{
		{"owner authority", spv2.HumanRef("someone"), true},
		{"runtime authority", spv2.AgentRef("reasoner").WithAuthority(spv2.AuthorityRuntime), true},
		{"agent id", spv2.AgentRef("reasoner"), true},
		{"entry id", spv2.ServiceRef("frontend"), true},
		{"outsider", spv2.ServiceRef("intruder"), false},
	} {
		err := authorizeWork(w, tc.from)
		if tc.ok && err != nil {
			t.Fatalf("%s 应允许: %v", tc.name, err)
		}
		if !tc.ok && !spv2.IsCode(err, spv2.CodePermissionDenied) {
			t.Fatalf("%s 应 permission_denied: %v", tc.name, err)
		}
	}
}

// context 组装必须先把 work 登记进账本；后续依赖（长期记忆/共享库）未就绪，
// 允许它继续失败，但 work 行必须在失败点之前就已落库（fail-closed 的前提）。
func TestAssembleRegistersWorkBeforeDependencies(t *testing.T) {
	s, st := newWorkTestServer(t)
	s.logger = log.New(io.Discard, "", 0)
	tc := protocol.TaskContext{
		ConversationID: "conv",
		Boss:           "frontend",
		Messages:       []protocol.Message{{Role: "user", Content: "跑一轮"}},
	}
	func() {
		defer func() { _ = recover() }() // 未启动的服务依赖可能 panic；work 登记在更早发生
		_, _, _ = s.assemble(context.Background(), tc, callMeta{from: "reasoner", trace: "w-recall"})
	}()
	w, err := st.GetWork(context.Background(), "w-recall")
	if err != nil {
		t.Fatalf("assemble 没有登记 work: %v", err)
	}
	if w.Agent.ID != "reasoner" || w.Entry.ID != "frontend" {
		t.Fatalf("work 归属不对: %+v", w)
	}
}

// 周期 sweep 的同包证据：过期未终态 work 必须变 stalled，并记 sump_work_stalled_total{reason=deadline}。
func TestSweepWorksMarksStalledAndCounts(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	past := time.Now().Add(-time.Minute).UTC().Format(time.RFC3339)
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID: "w-expired", Root: "w-expired", Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
		Lease: spv2.Lease{DeadlineAt: past},
	}); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Store: st}, log.New(io.Discard, "", 0))
	if n := s.sweepWorks(ctx); n != 1 {
		t.Fatalf("sweep 数量 = %d, want 1", n)
	}
	w, err := st.GetWork(ctx, "w-expired")
	if err != nil {
		t.Fatal(err)
	}
	if w.Status != spv2.WorkStalled {
		t.Fatalf("过期 work 状态 = %q, want stalled", w.Status)
	}
	// 指标断言放在真启动服务的 tests 包用例里（未 Start 的服务没有指标注册表）。
}

// §12.3 队列指标的同包证据：recordQueueDepth 必须把真实队列状态写进注册表。
func TestRecordQueueDepthMetric(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if _, err := st.EnsureWork(ctx, spv2.Work{
		ID: "w-depth", Root: "w-depth", Class: spv2.ClassRequest,
		Owner: spv2.HumanRef("owner"), Agent: spv2.AgentRef("reasoner"), Entry: spv2.ServiceRef("frontend"),
		Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PostWorkMessage(ctx, "w-depth", spv2.AgentRef("a"), spv2.AgentRef("b"), "message", "x"); err != nil {
		t.Fatal(err)
	}
	s := New(Config{Store: st}, log.New(io.Discard, "", 0))
	reg := spv2.NewRegistry()
	s.recordQueueDepthTo(reg, ctx)
	var pending float64 = -1
	for _, m := range reg.Snapshot() {
		if m.Name == "sump_queue_depth" && m.Labels["state"] == store.WorkMsgPending {
			pending = m.Value
		}
	}
	if pending != 1 {
		t.Fatalf("sump_queue_depth{pending} = %v, want 1（snapshot=%+v）", pending, reg.Snapshot())
	}
}
