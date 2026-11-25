package session

// 周期 call 对账的单测（S-001 §3.5 / R-029-D4）：只读 reconciler 的保守合并、
// recovery_complete 前提、orphan 判定、created 到期、owner 离线退避与批量上限。
// 全部用 fake OwnerClient + 临时库，离线可跑；不启动服务骨架（metrics 直接注入 Registry）。

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// fakeReconcileOwner 是只实现 Status 的假 owner；控制方法被调到即证明 reconciler 越界。
type fakeReconcileOwner struct {
	mu sync.Mutex

	states   map[string]string
	complete bool
	err      error

	statusCalls int
	chunks      [][]string
	services    []string

	controlCalls int
}

func (f *fakeReconcileOwner) Status(_ context.Context, service string, ids []string) (map[string]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	f.services = append(f.services, service)
	f.chunks = append(f.chunks, append([]string(nil), ids...))
	if f.err != nil {
		return nil, false, f.err
	}
	out := map[string]string{}
	for _, id := range ids {
		if exec, ok := f.states[id]; ok {
			out[id] = exec
		}
	}
	return out, f.complete, nil
}

func (f *fakeReconcileOwner) Pause(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controlCalls++
	return "", errors.New("对账不得下发控制")
}

func (f *fakeReconcileOwner) Resume(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controlCalls++
	return "", errors.New("对账不得下发控制")
}

func (f *fakeReconcileOwner) Cancel(context.Context, string, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controlCalls++
	return "", errors.New("对账不得下发控制")
}

func (f *fakeReconcileOwner) set(err error, states map[string]string, complete bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.err, f.states, f.complete = err, states, complete
}

func (f *fakeReconcileOwner) snapshot() (calls int, chunks [][]string, services []string, controls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.chunks))
	copy(out, f.chunks)
	return f.statusCalls, out, append([]string(nil), f.services...), f.controlCalls
}

// newReconcileServer 建一个未启动的 session 实例（reconciler 只需要 Store + 日志）。
func newReconcileServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}, st
}

// openReconcileCall 按 owner service 开一条 call（reconciler 不关心 work 归属之外的东西）。
func openReconcileCall(t *testing.T, s *Server, work, service string, execDeadlineMS int64) string {
	t.Helper()
	w, err := s.cfg.Store.GetWork(context.Background(), work)
	if err != nil {
		t.Fatalf("读取 work %s: %v", work, err)
	}
	res, err := s.openCall(context.Background(), callOpenBody(t, protocol.CallOpenPayload{
		Work: work, Service: service, Action: "run", Mode: store.CallModeOnce,
		Pausable: true, TTLMS: 60000, ExecDeadlineMS: execDeadlineMS,
	}), caller{work: work, from: spv2.AgentRef(w.Agent.ID)})
	if err != nil {
		t.Fatalf("openCall(%s/%s): %v", work, service, err)
	}
	return res.CallID
}

// markReconcileRunning 把 call 推进到 running 并回填受理句柄。
func markReconcileRunning(t *testing.T, st *store.Store, id, remote string) {
	t.Helper()
	if err := st.SetCallState(context.Background(), id, store.CallObserveWaiting,
		store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatalf("置 running: %v", err)
	}
	if err := st.SetCallRemote(context.Background(), id, remote); err != nil {
		t.Fatalf("回填受理句柄: %v", err)
	}
}

// markReconcilePaused 直接落 paused（模拟 owner 已确认过的既有状态）。
func markReconcilePaused(t *testing.T, st *store.Store, id, remote string) {
	t.Helper()
	markReconcileRunning(t, st, id, remote)
	if err := st.SetCallState(context.Background(), id, store.CallObserveNotWaiting,
		store.CallExecPaused, "", "pause_confirmed"); err != nil {
		t.Fatalf("置 paused: %v", err)
	}
}

// backdateCreated 把 created_at 改到过去（不需要等真实的 exec_deadline）。
func backdateCreated(t *testing.T, st *store.Store, id string, d time.Duration) {
	t.Helper()
	at := time.Now().Add(-d).UTC().Format(time.RFC3339Nano)
	if _, err := st.DB().Exec(`UPDATE calls SET created_at = ? WHERE id = ?`, at, id); err != nil {
		t.Fatalf("回拨 created_at: %v", err)
	}
}

func reconcileRow(t *testing.T, st *store.Store, id string) store.CallRow {
	t.Helper()
	row, err := st.GetCall(context.Background(), id)
	if err != nil {
		t.Fatalf("读 call %s: %v", id, err)
	}
	return row
}

// reconcileMetric 取一个指标的当前值（labels 必须全部匹配；nil labels = 无标签）。
func reconcileMetric(reg *spv2.Registry, name string, labels map[string]string) float64 {
	for _, m := range reg.Snapshot() {
		if m.Name != name || len(m.Labels) != len(labels) {
			continue
		}
		match := true
		for k, v := range labels {
			if m.Labels[k] != v {
				match = false
				break
			}
		}
		if match {
			return m.Value
		}
	}
	return 0
}

// TestReconcileMergesOwnerExec：owner 报出的执行维按状态机落库；控制方法一次都不调。
func TestReconcileMergesOwnerExec(t *testing.T) {
	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	fake := &fakeReconcileOwner{
		complete: true,
		states: map[string]string{
			"r-done":    protocol.OwnerExecDone,
			"r-failed":  protocol.OwnerExecFailed,
			"r-paused":  protocol.OwnerExecPaused,
			"r-unknown": protocol.OwnerExecUnknown,
			"r-running": protocol.OwnerExecRunning,
		},
	}
	s.SetOwnerClient(fake)

	ids := map[string]string{}
	for remote := range fake.states {
		id := openReconcileCall(t, s, "w1", "svc", 300000)
		markReconcileRunning(t, st, id, remote)
		ids[remote] = id
	}

	reg := spv2.NewRegistry()
	s.reconcileCallsTo(context.Background(), reg)

	cases := []struct {
		remote, observe, exec, phase string
	}{
		{"r-done", store.CallObserveClosed, store.CallExecDone, store.CallPhaseDone},
		{"r-failed", store.CallObserveClosed, store.CallExecFailed, store.CallPhaseFailed},
		{"r-paused", store.CallObserveNotWaiting, store.CallExecPaused, ""},
		{"r-unknown", store.CallObserveNotWaiting, store.CallExecUnknown, ""},
		{"r-running", store.CallObserveWaiting, store.CallExecRunning, ""},
	}
	for _, c := range cases {
		row := reconcileRow(t, st, ids[c.remote])
		if row.Observe != c.observe || row.Exec != c.exec || row.Phase != c.phase {
			t.Fatalf("%s 的落点不对: %+v（want %s/%s/%s）", c.remote, row, c.observe, c.exec, c.phase)
		}
	}
	if got := reconcileMetric(reg, reconcileScannedMetric, nil); got != 5 {
		t.Fatalf("扫描数 = %v, want 5", got)
	}
	if got := reconcileMetric(reg, reconcileUpdatedMetric, nil); got != 4 {
		t.Fatalf("更新数 = %v, want 4（running 本条无变化）", got)
	}
	if _, _, _, controls := fake.snapshot(); controls != 0 {
		t.Fatalf("对账不得下发控制（调用 %d 次）", controls)
	}
}

// TestReconcileRecoveryIncompleteDoesNotOrphan：recovery_complete=false 时「不认识」不判 orphaned；
// 恢复完整后同一批才落 orphaned。
func TestReconcileRecoveryIncompleteDoesNotOrphan(t *testing.T) {
	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	fake := &fakeReconcileOwner{complete: false}
	s.SetOwnerClient(fake)

	id := openReconcileCall(t, s, "w1", "svc", 300000)
	markReconcileRunning(t, st, id, "r-lost")

	reg := spv2.NewRegistry()
	s.reconcileCallsTo(context.Background(), reg)
	if row := reconcileRow(t, st, id); row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("恢复未完成时不得判 orphaned: %+v", row)
	}
	if got := reconcileMetric(reg, reconcileUpdatedMetric, nil); got != 0 {
		t.Fatalf("恢复未完成时不该改写任何 call（updated=%v）", got)
	}

	fake.set(nil, nil, true)
	s.reconcileCallsTo(context.Background(), reg)
	row := reconcileRow(t, st, id)
	if row.Observe != store.CallObserveNotWaiting || row.Exec != store.CallExecUnknown || row.Phase != store.CallPhaseOrphaned {
		t.Fatalf("恢复完整且 owner 不认识时应落 orphaned: %+v", row)
	}
}

// TestReconcileOrphanOnlyWithRemoteHandle：只有「有受理句柄、owner 恢复完整、且不是 created」
// 的 call 才判 orphaned；没有句柄的 call 保持本地观测，created 交给到期规则。
func TestReconcileOrphanOnlyWithRemoteHandle(t *testing.T) {
	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	fake := &fakeReconcileOwner{complete: true}
	s.SetOwnerClient(fake)

	running := openReconcileCall(t, s, "w1", "svc", 300000)
	markReconcileRunning(t, st, running, "r-running")
	paused := openReconcileCall(t, s, "w1", "svc", 300000)
	markReconcilePaused(t, st, paused, "r-paused")
	createdWithHandle := openReconcileCall(t, s, "w1", "svc", 300000)
	if err := st.SetCallRemote(context.Background(), createdWithHandle, "r-created"); err != nil {
		t.Fatal(err)
	}
	noHandle := openReconcileCall(t, s, "w1", "svc", 300000)
	if err := st.SetCallState(context.Background(), noHandle, store.CallObserveWaiting,
		store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatal(err)
	}
	createdNoHandle := openReconcileCall(t, s, "w1", "svc", 300000)

	s.reconcileCallsTo(context.Background(), spv2.NewRegistry())

	for _, id := range []string{running, paused} {
		row := reconcileRow(t, st, id)
		if row.Exec != store.CallExecUnknown || row.Phase != store.CallPhaseOrphaned || row.Observe != store.CallObserveNotWaiting {
			t.Fatalf("%s 应落 orphaned: %+v", id, row)
		}
	}
	if row := reconcileRow(t, st, createdWithHandle); row.Exec != store.CallExecCreated || row.Phase != "" {
		t.Fatalf("created 不判 orphaned（交给到期规则）: %+v", row)
	}
	if row := reconcileRow(t, st, noHandle); row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("没有受理句柄的 call 不判 orphaned: %+v", row)
	}
	if row := reconcileRow(t, st, createdNoHandle); row.Exec != store.CallExecCreated || row.Phase != "" {
		t.Fatalf("created 没有句柄时保持本地意图: %+v", row)
	}
}

// TestReconcileCreatedExpires：created 过 exec_deadline_ms → unknown + dispatch_unconfirmed，
// 不写 phase、不转 cancelled；没有到期约束（0）或未到期的 created 不动。
func TestReconcileCreatedExpires(t *testing.T) {
	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	s.SetOwnerClient(&fakeReconcileOwner{complete: true})

	expired := openReconcileCall(t, s, "w1", "svc", 1000)
	backdateCreated(t, st, expired, time.Hour)
	notYet := openReconcileCall(t, s, "w1", "svc", (24 * time.Hour).Milliseconds())
	noDeadline := openReconcileCall(t, s, "w1", "svc", 0)
	backdateCreated(t, st, noDeadline, 48*time.Hour)

	reg := spv2.NewRegistry()
	s.reconcileCallsTo(context.Background(), reg)

	row := reconcileRow(t, st, expired)
	if row.Observe != store.CallObserveNotWaiting || row.Exec != store.CallExecUnknown ||
		row.Phase != "" || row.Reason != "dispatch_unconfirmed" {
		t.Fatalf("到期的 created 应转 unknown/dispatch_unconfirmed（不写 phase）: %+v", row)
	}
	if row.Exec == store.CallExecCancelled {
		t.Fatal("派发未确认不得转 cancelled")
	}
	for _, id := range []string{notYet, noDeadline} {
		if row := reconcileRow(t, st, id); row.Exec != store.CallExecCreated || row.Reason != "" {
			t.Fatalf("%s 不该被到期规则改写: %+v", id, row)
		}
	}
	if got := reconcileMetric(reg, reconcileUpdatedMetric, nil); got != 1 {
		t.Fatalf("更新数 = %v, want 1", got)
	}
}

// TestReconcileCreatedTerminalViaAcceptance：owner 第一次就答终态时，先经 running 一跳再合并；
// 矛盾答案（本地 running、owner 答 created）不绕过状态机。
func TestReconcileCreatedTerminalViaAcceptance(t *testing.T) {
	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	fake := &fakeReconcileOwner{complete: true, states: map[string]string{
		"r-done":    protocol.OwnerExecDone,
		"r-created": protocol.OwnerExecCreated,
	}}
	s.SetOwnerClient(fake)

	createdDone := openReconcileCall(t, s, "w1", "svc", 300000)
	if err := st.SetCallRemote(context.Background(), createdDone, "r-done"); err != nil {
		t.Fatal(err)
	}
	running := openReconcileCall(t, s, "w1", "svc", 300000)
	markReconcileRunning(t, st, running, "r-created")

	s.reconcileCallsTo(context.Background(), spv2.NewRegistry())

	if row := reconcileRow(t, st, createdDone); row.Observe != store.CallObserveClosed ||
		row.Exec != store.CallExecDone || row.Phase != store.CallPhaseDone {
		t.Fatalf("created + owner 答 done 应先补受理再落终态: %+v", row)
	}
	if row := reconcileRow(t, st, running); row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("owner 答 created 不得把 running 回退: %+v", row)
	}
}

// TestReconcileOwnerBackoff：owner 离线后按指数退避跳过，成功一轮后恢复。
func TestReconcileOwnerBackoff(t *testing.T) {
	oldBase, oldCap := ownerReconcileBackoffBase, ownerReconcileBackoffCap
	ownerReconcileBackoffBase, ownerReconcileBackoffCap = 200*time.Millisecond, 400*time.Millisecond
	t.Cleanup(func() { ownerReconcileBackoffBase, ownerReconcileBackoffCap = oldBase, oldCap })

	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	fake := &fakeReconcileOwner{err: errors.New("owner 离线")}
	s.SetOwnerClient(fake)
	id := openReconcileCall(t, s, "w1", "svc", 300000)
	markReconcileRunning(t, st, id, "r1")

	reg := spv2.NewRegistry()
	s.reconcileCallsTo(context.Background(), reg)
	if calls, _, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("首轮应问 owner 一次，实际 %d", calls)
	}
	if got := reconcileMetric(reg, reconcileOwnerOfflineMetric, map[string]string{"service": "svc"}); got != 1 {
		t.Fatalf("离线指标 = %v, want 1", got)
	}
	if row := reconcileRow(t, st, id); row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("owner 离线不得改写本地状态: %+v", row)
	}

	// 退避窗口内：不再打 owner。
	s.reconcileCallsTo(context.Background(), reg)
	if calls, _, _, _ := fake.snapshot(); calls != 1 {
		t.Fatalf("退避窗口内不应再问 owner，实际 %d 次", calls)
	}

	// 窗口过后 + owner 恢复：合并并清掉退避。
	time.Sleep(250 * time.Millisecond)
	fake.set(nil, map[string]string{"r1": protocol.OwnerExecDone}, true)
	s.reconcileCallsTo(context.Background(), reg)
	if calls, _, _, _ := fake.snapshot(); calls != 2 {
		t.Fatalf("窗口过后应再问 owner，实际 %d 次", calls)
	}
	if row := reconcileRow(t, st, id); row.Exec != store.CallExecDone || row.Observe != store.CallObserveClosed {
		t.Fatalf("owner 恢复后应按答案合并: %+v", row)
	}
	if s.ownerBackoffActive("svc", time.Now()) {
		t.Fatal("成功一轮后应清掉退避")
	}
}

// TestOwnerReconcileBackoffSchedule：退避时长本身（基 30s、指数、封顶 60s）。
func TestOwnerReconcileBackoffSchedule(t *testing.T) {
	oldBase, oldCap := ownerReconcileBackoffBase, ownerReconcileBackoffCap
	ownerReconcileBackoffBase, ownerReconcileBackoffCap = 30*time.Second, 60*time.Second
	t.Cleanup(func() { ownerReconcileBackoffBase, ownerReconcileBackoffCap = oldBase, oldCap })

	cases := map[int]time.Duration{0: 0, 1: 30 * time.Second, 2: 60 * time.Second, 3: 60 * time.Second, 10: 60 * time.Second}
	for failures, want := range cases {
		if got := ownerReconcileBackoff(failures); got != want {
			t.Fatalf("failures=%d 的退避 = %s, want %s", failures, got, want)
		}
	}
}

// TestReconcileBatchSize：每 owner_service 每批最多 100 个句柄。
func TestReconcileBatchSize(t *testing.T) {
	s, st := newReconcileServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	fake := &fakeReconcileOwner{complete: true, states: map[string]string{}}
	s.SetOwnerClient(fake)

	const n = 205
	for i := 0; i < n; i++ {
		remote := "r-" + strconv.Itoa(i)
		id := openReconcileCall(t, s, "w1", "svc", 300000)
		markReconcileRunning(t, st, id, remote)
		fake.states[remote] = protocol.OwnerExecRunning
	}

	s.reconcileCallsTo(context.Background(), spv2.NewRegistry())
	calls, chunks, services, _ := fake.snapshot()
	if calls != 3 || len(chunks) != 3 {
		t.Fatalf("205 个句柄应分 3 批，实际 calls=%d chunks=%d", calls, len(chunks))
	}
	for i, want := range []int{100, 100, 5} {
		if len(chunks[i]) != want {
			t.Fatalf("第 %d 批 = %d 个句柄, want %d", i+1, len(chunks[i]), want)
		}
	}
	for _, svc := range services {
		if svc != "svc" {
			t.Fatalf("对账应按 call 的 owner service 分批，实际 %q", svc)
		}
	}
}
