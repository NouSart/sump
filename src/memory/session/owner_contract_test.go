package session

// owner 契约测试（S-001 §3.4 控制与握手 / §3.5 对账 / §5.4 崩溃与离线；
// §10 fake owner 矩阵里属于 C 切片的部分）。
//
// 用可配置的 fake owner 覆盖故障矩阵：pause 成功 / 失败 / ACK 丢失 / 超时 / owner 回答还在跑，
// resume 成功 / 失败，cancel 可暂停成功 / 未确认 / owner 回答还在跑 / 不可暂停，
// call_status 的 recovery_complete=false/true、未知 id、owner 离线，以及 nil 客户端的诚实行为。
//
// 与源 af6cfc8:src/memory/server/owner_contract_test.go 的差异（R-023 P3）：
//   - 跨 work 归属用例（TestOwnerContractCallAuthorization）留到 P4：本阶段不接授权，
//     控制 / 对账只按载荷 work 与 call_id 操作；
//   - 补一个本阶段真实的 owner 三态用例：owner 明确回答「还在跑」时 pause 不得写 paused
//     （源只覆盖了 cancel 的同形情形），保持 13 个用例；
//   - handler 的线上收发需要真启动服务，属于端到端范围；这里同包直调逻辑层
//     （controlCall / callStatus），存储用临时库，逐条校验状态落点。

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// fakeOwner 是可配置的假执行服务。
//
// ownerState 记录 fake 自己的「执行真相」：ACK 丢失用例里 fake 返回错误但内部已暂停，
// 用来证明 session 不会把 owner 侧的事实当成自己已经确认。
type fakeOwner struct {
	mu sync.Mutex

	// call_status
	statusStates   map[string]string // owner 认识（且愿意回答）的 call → exec
	statusRecovery bool              // owner 持久受理记录是否已恢复完整
	statusErr      error             // 非 nil = owner 离线 / 对账失败
	statusCalls    int
	statusServices []string

	// pause
	pauseExec    string // 成功时返回的 exec（空 = 按约定 "paused"）
	pauseErr     error
	pauseTimeout bool // true = 挂到 ctx 结束（模拟 ACK 超时）
	pauseApplied bool // true 且返回错误：owner 内部其实已暂停（模拟 ACK 丢失）
	pauseCalls   int

	// resume
	resumeExec  string // 成功时返回的 exec（空 = 按约定 "running"）
	resumeErr   error
	resumeCalls int

	// cancel
	cancelExec  string // 成功时返回的 exec（空 = 按约定 "cancelled"）
	cancelErr   error
	cancelCalls int

	ownerState map[string]string // fake 侧的执行真相（call_id → exec）
}

func (f *fakeOwner) Status(_ context.Context, service string, ids []string) (map[string]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusCalls++
	f.statusServices = append(f.statusServices, service)
	if f.statusErr != nil {
		return nil, false, f.statusErr
	}
	out := map[string]string{}
	for _, id := range ids {
		if exec, ok := f.statusStates[id]; ok {
			out[id] = exec
		}
	}
	return out, f.statusRecovery, nil
}

func (f *fakeOwner) Pause(ctx context.Context, _ string, callID string) (string, error) {
	f.mu.Lock()
	f.pauseCalls++
	timeout, exec, err, applied := f.pauseTimeout, f.pauseExec, f.pauseErr, f.pauseApplied
	f.mu.Unlock()

	if timeout {
		<-ctx.Done()
		return "", ctx.Err()
	}
	if err != nil {
		if applied {
			f.setOwnerState(callID, store.CallExecPaused)
		}
		return "", err
	}
	if exec == "" {
		exec = store.CallExecPaused
	}
	f.setOwnerState(callID, exec)
	return exec, nil
}

func (f *fakeOwner) Resume(_ context.Context, _ string, callID string) (string, error) {
	f.mu.Lock()
	f.resumeCalls++
	exec, err := f.resumeExec, f.resumeErr
	f.mu.Unlock()

	if err != nil {
		return "", err
	}
	if exec == "" {
		exec = store.CallExecRunning
	}
	f.setOwnerState(callID, exec)
	return exec, nil
}

func (f *fakeOwner) Cancel(_ context.Context, _ string, callID string) (string, error) {
	f.mu.Lock()
	f.cancelCalls++
	exec, err := f.cancelExec, f.cancelErr
	f.mu.Unlock()

	if err != nil {
		return "", err
	}
	if exec == "" {
		exec = store.CallExecCancelled
	}
	f.setOwnerState(callID, exec)
	return exec, nil
}

func (f *fakeOwner) setOwnerState(callID, exec string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ownerState == nil {
		f.ownerState = map[string]string{}
	}
	f.ownerState[callID] = exec
}

// state 返回 fake 侧对某 call 的执行真相（用于 ACK 丢失用例）。
func (f *fakeOwner) state(callID string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ownerState[callID]
}

// counts 返回各控制操作与对账的调用次数（验证幂等与「不可暂停不下发控制」）。
func (f *fakeOwner) counts() (pause, resume, cancel, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pauseCalls, f.resumeCalls, f.cancelCalls, f.statusCalls
}

// lastStatusServices 返回对账问过的 owner service（去重后的顺序不重要，调用方用集合判断）。
func (f *fakeOwner) lastStatusServices() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusServices...)
}

// ---------- 测试脚手架 ----------

// newOwnerContractServer 建一个注入了 fake owner 的 session 实例（临时库）。
// 用 New 构造：owner 客户端经 Config 注入，顺带证明生产装配路径能把它带上。
func newOwnerContractServer(t *testing.T, owner OwnerClient) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return New(Config{Store: st, OwnerClient: owner}, log.New(io.Discard, "", 0)), st
}

// dispatchCallsTestCall 把 call_open 的 created 推进到 running（模拟已派发给 owner）。
func dispatchCallsTestCall(t *testing.T, st *store.Store, id string) {
	t.Helper()
	if err := st.SetCallState(context.Background(), id, store.CallObserveWaiting,
		store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatalf("置 running: %v", err)
	}
}

// markCallsTestPaused 把调用置成 paused（模拟 owner 已确认过暂停的既有状态）。
func markCallsTestPaused(t *testing.T, st *store.Store, id string) {
	t.Helper()
	dispatchCallsTestCall(t, st, id)
	if err := st.SetCallState(context.Background(), id, store.CallObserveWaiting,
		store.CallExecPaused, "", "pause_confirmed"); err != nil {
		t.Fatalf("置 paused: %v", err)
	}
}

func mustCallRow(t *testing.T, st *store.Store, id string) store.CallRow {
	t.Helper()
	row, err := st.GetCall(context.Background(), id)
	if err != nil {
		t.Fatalf("读 call %s: %v", id, err)
	}
	return row
}

// ---------- pause ----------

func TestOwnerContractPauseConfirmed(t *testing.T) {
	fake := &fakeOwner{}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause", Op: "pause", TTLMS: 1000,
	})
	if err != nil {
		t.Fatalf("pause 成功路径不应报错: %v", err)
	}
	if res.Observe != store.CallObserveNotWaiting || res.Exec != store.CallExecPaused || res.Reason != "pause_confirmed" {
		t.Fatalf("pause 确认后的两维不对: %+v", res)
	}
	row := mustCallRow(t, st, id)
	if row.Observe != store.CallObserveNotWaiting || row.Exec != store.CallExecPaused || row.Phase != "" {
		t.Fatalf("pause 确认后的落库不对: %+v", row)
	}
	if fake.state(id) != store.CallExecPaused {
		t.Fatalf("fake owner 侧应为 paused: %q", fake.state(id))
	}

	// 同 control_id 重放：直接回首次结果，不再打 owner（幂等）。
	replay, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause", Op: "pause", TTLMS: 1000,
	})
	if err != nil {
		t.Fatalf("同 control_id 重放应成功: %v", err)
	}
	if replay.Observe != res.Observe || replay.Exec != res.Exec || replay.Reason != res.Reason {
		t.Fatalf("重放结果不一致: %+v vs %+v", replay, res)
	}
	if p, _, _, _ := fake.counts(); p != 1 {
		t.Fatalf("重放不应再次调用 owner Pause（调用 %d 次）", p)
	}

	// created（未受理）的调用不做 pause 握手：那是本地意图，还没有 owner 可确认。
	id2 := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id2, ControlID: "ctl-pause-created", Op: "pause"}); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("created 上 pause 应 conflict: %v", err)
	}
	if p, _, _, _ := fake.counts(); p != 1 {
		t.Fatalf("created 的 pause 不应下发 owner（Pause 调用 %d 次）", p)
	}
}

func TestOwnerContractPauseFailureKeepsExec(t *testing.T) {
	fake := &fakeOwner{pauseErr: errors.New("owner 拒绝暂停")}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause-fail", Op: "pause", TTLMS: 1000,
	})
	if err != nil {
		t.Fatalf("pause 失败应回可观测结果而不是协议错误: %v", err)
	}
	if res.Observe != store.CallObserveNotWaiting || res.Exec != store.CallExecRunning || res.Reason != "pause_failed" {
		t.Fatalf("pause 失败的结果不对: %+v", res)
	}
	// 关键：执行维保持 running，phase 仍为空——不能因为没确认就写 paused / 终态。
	row := mustCallRow(t, st, id)
	if row.Observe != store.CallObserveNotWaiting || row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("pause 失败不得改执行维: %+v", row)
	}
	if fake.state(id) != "" {
		t.Fatalf("owner 未确认暂停，fake 侧不应有 paused 真相: %q", fake.state(id))
	}

	// 失败回执也落 control_id：同 control_id 重放回同一观测，不再打 owner。
	replay, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause-fail", Op: "pause", TTLMS: 1000,
	})
	if err != nil || replay.Reason != "pause_failed" || replay.Exec != store.CallExecRunning {
		t.Fatalf("pause 失败回执重放不对: %+v, err=%v", replay, err)
	}
	if p, _, _, _ := fake.counts(); p != 1 {
		t.Fatalf("重放不应再次调用 owner Pause（调用 %d 次）", p)
	}
}

func TestOwnerContractPauseAckLostKeepsState(t *testing.T) {
	// fake 返回错误，但 owner 内部其实已经暂停（ACK 丢失）。
	fake := &fakeOwner{pauseErr: errors.New("pause ACK 丢失"), pauseApplied: true}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause-lost", Op: "pause", TTLMS: 1000,
	})
	if err != nil {
		t.Fatalf("ACK 丢失应回可观测结果: %v", err)
	}
	if res.Reason != "pause_failed" || res.Exec != store.CallExecRunning {
		t.Fatalf("ACK 丢失的结果不对: %+v", res)
	}
	if got := fake.state(id); got != store.CallExecPaused {
		t.Fatalf("fake owner 内部应已暂停（前提不成立）: %q", got)
	}
	// INV-13 / S-001 §3.4：没收到 ACK 不能证明已暂停，session 不得写 paused，也不写终态 phase。
	row := mustCallRow(t, st, id)
	if row.Exec == store.CallExecPaused {
		t.Fatalf("ACK 丢失不得写 paused: %+v", row)
	}
	if row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("ACK 丢失后执行维/phase 应保持原值: %+v", row)
	}
}

func TestOwnerContractPauseTimeout(t *testing.T) {
	fake := &fakeOwner{pauseTimeout: true}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	start := time.Now()
	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause-timeout", Op: "pause", TTLMS: 20,
	})
	if err != nil {
		t.Fatalf("pause 超时应回可观测结果而不是协议错误: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("pause 超时未按 ttl_ms 返回（等了 %v）", elapsed)
	}
	if res.Reason != "pause_failed" || res.Observe != store.CallObserveNotWaiting || res.Exec != store.CallExecRunning {
		t.Fatalf("pause 超时结果不对: %+v", res)
	}
	row := mustCallRow(t, st, id)
	if row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("pause 超时不得写 paused / 终态 phase: %+v", row)
	}
}

// TestOwnerContractPauseOwnerAnswersRunning 是本阶段补的三态用例：
// owner 明确回答「还在跑」（exec=running）不是暂停确认，不能记成 paused。
func TestOwnerContractPauseOwnerAnswersRunning(t *testing.T) {
	fake := &fakeOwner{pauseExec: store.CallExecRunning}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-pause-running", Op: "pause", TTLMS: 1000,
	})
	if err != nil {
		t.Fatalf("owner 的回答是可观测结果，不应变成协议错误: %v", err)
	}
	if res.Observe != store.CallObserveNotWaiting || res.Exec != store.CallExecRunning || res.Reason != "pause_failed" {
		t.Fatalf("owner 回答 running 必须按未确认处理: %+v", res)
	}
	row := mustCallRow(t, st, id)
	if row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("未确认的 pause 落库不对: %+v", row)
	}
	if got := fake.state(id); got != store.CallExecRunning {
		t.Fatalf("前提：fake 侧应保持在跑（实际 %q）", got)
	}
}

// ---------- resume ----------

func TestOwnerContractResume(t *testing.T) {
	t.Run("确认成功", func(t *testing.T) {
		fake := &fakeOwner{}
		s, st := newOwnerContractServer(t, fake)
		seedCallsWork(t, st, "w1", "alice", "reasoner")
		id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
		markCallsTestPaused(t, st, id)

		res, err := controlCalls(t, s, protocol.CallControlPayload{
			ID: id, ControlID: "ctl-resume", Op: "resume", TTLMS: 1000,
		})
		if err != nil {
			t.Fatalf("resume 成功路径不应报错: %v", err)
		}
		if res.Observe != store.CallObserveWaiting || res.Exec != store.CallExecRunning || res.Reason != "resume_confirmed" {
			t.Fatalf("resume 确认后的两维不对: %+v", res)
		}
		row := mustCallRow(t, st, id)
		if row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecRunning || row.Phase != "" {
			t.Fatalf("resume 确认后的落库不对: %+v", row)
		}
		if got := fake.state(id); got != store.CallExecRunning {
			t.Fatalf("fake owner 侧应为 running: %q", got)
		}
	})

	t.Run("确认失败保持paused", func(t *testing.T) {
		fake := &fakeOwner{resumeErr: errors.New("owner 离线")}
		s, st := newOwnerContractServer(t, fake)
		seedCallsWork(t, st, "w1", "alice", "reasoner")
		id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
		markCallsTestPaused(t, st, id)

		res, err := controlCalls(t, s, protocol.CallControlPayload{
			ID: id, ControlID: "ctl-resume-fail", Op: "resume", TTLMS: 1000,
		})
		if err != nil {
			t.Fatalf("resume 失败应回可观测结果而不是协议错误: %v", err)
		}
		if res.Exec != store.CallExecPaused || res.Reason != "resume_failed" {
			t.Fatalf("resume 失败的结果不对: %+v", res)
		}
		row := mustCallRow(t, st, id)
		if row.Exec != store.CallExecPaused || row.Observe != store.CallObserveWaiting || row.Phase != "" {
			t.Fatalf("resume 失败不得改状态: %+v", row)
		}
	})
}

// ---------- cancel ----------

func TestOwnerContractCancel(t *testing.T) {
	t.Run("可暂停且owner确认", func(t *testing.T) {
		fake := &fakeOwner{}
		s, st := newOwnerContractServer(t, fake)
		seedCallsWork(t, st, "w1", "alice", "reasoner")
		id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
		dispatchCallsTestCall(t, st, id)

		res, err := controlCalls(t, s, protocol.CallControlPayload{
			ID: id, ControlID: "ctl-cancel", Op: "cancel", TTLMS: 1000,
		})
		if err != nil {
			t.Fatalf("cancel 确认路径不应报错: %v", err)
		}
		if res.Observe != store.CallObserveClosed || res.Exec != store.CallExecCancelled || res.Reason != "cancelled" {
			t.Fatalf("cancel 确认结果不对: %+v", res)
		}
		row := mustCallRow(t, st, id)
		if row.Observe != store.CallObserveClosed || row.Exec != store.CallExecCancelled ||
			row.Phase != store.CallPhaseCancelled || row.RemoteMayContinue {
			t.Fatalf("cancel 确认后的落库不对: %+v", row)
		}
		if _, _, c, _ := fake.counts(); c != 1 {
			t.Fatalf("可暂停调用应下发 owner Cancel（调用 %d 次）", c)
		}
	})

	t.Run("可暂停但owner未确认", func(t *testing.T) {
		// owner 超时 / 离线：取消未获确认，远端可能还在跑。
		fake := &fakeOwner{cancelErr: errors.New("cancel ACK 丢失")}
		s, st := newOwnerContractServer(t, fake)
		seedCallsWork(t, st, "w1", "alice", "reasoner")
		id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
		dispatchCallsTestCall(t, st, id)

		res, err := controlCalls(t, s, protocol.CallControlPayload{
			ID: id, ControlID: "ctl-cancel-lost", Op: "cancel", TTLMS: 1000,
		})
		if err != nil {
			t.Fatalf("cancel 未确认应回可观测结果: %v", err)
		}
		if res.Observe != store.CallObserveClosed || res.Exec != store.CallExecUnknown || res.Reason != "cancel_unconfirmed" {
			t.Fatalf("cancel 未确认结果不对: %+v", res)
		}
		row := mustCallRow(t, st, id)
		if row.Observe != store.CallObserveClosed || row.Exec != store.CallExecUnknown ||
			row.Phase != "" || !row.RemoteMayContinue {
			t.Fatalf("cancel 未确认的落库不对（不写 phase、标 remote_may_continue）: %+v", row)
		}
	})

	t.Run("不可暂停不下发控制", func(t *testing.T) {
		fake := &fakeOwner{}
		s, st := newOwnerContractServer(t, fake)
		seedCallsWork(t, st, "w1", "alice", "reasoner")
		id := openCallsTestCall(t, s, "w1", store.CallModeOnce, false)
		dispatchCallsTestCall(t, st, id)

		res, err := controlCalls(t, s, protocol.CallControlPayload{
			ID: id, ControlID: "ctl-cancel-local", Op: "cancel", TTLMS: 1000,
		})
		if err != nil {
			t.Fatalf("不可暂停取消不应报错: %v", err)
		}
		if res.Observe != store.CallObserveClosed || res.Exec != store.CallExecUnknown || res.Reason != "cancelled_local" {
			t.Fatalf("不可暂停取消结果不对: %+v", res)
		}
		row := mustCallRow(t, st, id)
		if row.Phase != "" || !row.RemoteMayContinue {
			t.Fatalf("不可暂停取消的落库不对: %+v", row)
		}
		// 发了也停不下来：不能假发控制帧。
		if _, _, c, _ := fake.counts(); c != 0 {
			t.Fatalf("不可暂停调用不应下发 owner Cancel（调用 %d 次）", c)
		}
	})
}

func TestOwnerContractCancelOwnerAnswersRunning(t *testing.T) {
	// 审计缺陷 3：owner 返回 exec=running 且没有报错，旧实现只看 err 就记成 cancelled。
	// 「我还在跑」不是取消确认，必须落到 unknown + remote_may_continue，且不写终态 phase。
	fake := &fakeOwner{cancelExec: store.CallExecRunning}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-cancel-running", Op: "cancel", TTLMS: 1000,
	})
	if err != nil {
		t.Fatalf("owner 的回答是可观测结果，不应变成协议错误: %v", err)
	}
	if res.Exec != store.CallExecUnknown || res.Reason != "cancel_unconfirmed" {
		t.Fatalf("owner 回答 running 必须按未确认处理: %+v", res)
	}
	row := mustCallRow(t, st, id)
	if row.Observe != store.CallObserveClosed || row.Exec != store.CallExecUnknown ||
		row.Phase != "" || !row.RemoteMayContinue {
		t.Fatalf("未确认的 cancel 落库不对: %+v", row)
	}
	if got := fake.state(id); got != store.CallExecRunning {
		t.Fatalf("前提：fake 侧应保持在跑（实际 %q）", got)
	}
	// 控制记录不能把未确认写成 cancelled：同 control_id 重放回同一结论。
	replay, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: id, ControlID: "ctl-cancel-running", Op: "cancel", TTLMS: 1000,
	})
	if err != nil || replay.Exec != store.CallExecUnknown || replay.Reason != "cancel_unconfirmed" {
		t.Fatalf("重放应保持未确认结论: %+v err=%v", replay, err)
	}
	if _, _, c, _ := fake.counts(); c != 1 {
		t.Fatalf("重放不应再次下发 owner Cancel（调用 %d 次）", c)
	}
}

// ---------- call_status ----------

func TestOwnerContractStatusIncompleteDoesNotOrphan(t *testing.T) {
	// recovery_complete=false（owner 启动早期）：owner 不认识该 call 也不能判 orphaned。
	fake := &fakeOwner{statusRecovery: false}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: []string{id, "missing-call"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.OwnerRecoveryComplete {
		t.Fatal("recovery_complete=false 必须显式上报 false")
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %+v", res.Items)
	}
	// 本地已知 call：保留本地观测，不判 orphaned。
	if res.Items[0].Exec != store.CallExecRunning || res.Items[0].Phase != "" {
		t.Fatalf("恢复未完成时不得判 orphaned: %+v", res.Items[0])
	}
	// 本地未知 id：只回 id，保持 unknown。
	if res.Items[1].ID != "missing-call" || res.Items[1].Exec != "" || res.Items[1].Phase != "" {
		t.Fatalf("未知 id 不得被判定: %+v", res.Items[1])
	}
}

func TestOwnerContractStatusCompleteMarksOrphaned(t *testing.T) {
	// recovery_complete=true 且 owner 明确不认识本地已知 call：执行真相丢失（§5.4）。
	fake := &fakeOwner{statusRecovery: true}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: []string{id, "missing-call"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OwnerRecoveryComplete {
		t.Fatal("owner 恢复完整时应上报 true")
	}
	if res.Items[0].Exec != store.CallExecUnknown || res.Items[0].Phase != store.CallPhaseOrphaned {
		t.Fatalf("owner 明确不认识时应报告 orphaned: %+v", res.Items[0])
	}
	// 只读对账：本地账本不被报告层改写。
	if row := mustCallRow(t, st, id); row.Exec != store.CallExecRunning || row.Phase != "" {
		t.Fatalf("对账报告不应改本地账本: %+v", row)
	}
	// 本地完全查不到的 id 没有 owner 可问：即便别的 service 恢复完整，也保持 unknown。
	if res.Items[1].Exec != "" || res.Items[1].Phase != "" {
		t.Fatalf("本地未知 id 不应被判 orphaned: %+v", res.Items[1])
	}
}

func TestOwnerContractStatusOwnerOfflineKeepsState(t *testing.T) {
	fake := &fakeOwner{statusErr: errors.New("owner 离线")}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	res, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: []string{id, "ghost-call"}})
	if err != nil {
		t.Fatalf("owner 离线不应让 call_status 整体失败: %v", err)
	}
	if res.OwnerRecoveryComplete {
		t.Fatal("owner 离线时 recovery_complete 必须为 false")
	}
	if res.Items[0].Exec != store.CallExecRunning || res.Items[0].Phase != "" {
		t.Fatalf("owner 离线时应保持本地观测、不判 orphaned: %+v", res.Items[0])
	}
	if res.Items[1].Exec != "" || res.Items[1].Phase != "" {
		t.Fatalf("owner 离线时未知 id 不得被判定: %+v", res.Items[1])
	}
	if row := mustCallRow(t, st, id); row.Exec != store.CallExecRunning {
		t.Fatalf("owner 离线不应改本地账本: %+v", row)
	}
}

func TestOwnerContractStatusReportsOwnerExecByService(t *testing.T) {
	fake := &fakeOwner{statusRecovery: true, statusStates: map[string]string{}}
	s, st := newOwnerContractServer(t, fake)
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	shellID := openCallsTestCall(t, s, "w1", store.CallModeOnce, true) // service=shell
	pyRes, err := s.openCall(context.Background(), callOpenBody(t, protocol.CallOpenPayload{
		Work: "w1", Service: "python", Action: "run", Mode: store.CallModeOnce,
	}))
	if err != nil {
		t.Fatal(err)
	}
	dispatchCallsTestCall(t, st, shellID)
	dispatchCallsTestCall(t, st, pyRes.CallID)

	fake.mu.Lock()
	fake.statusStates[shellID] = store.CallExecDone
	fake.statusStates[pyRes.CallID] = store.CallExecFailed
	fake.mu.Unlock()

	res, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: []string{shellID, pyRes.CallID}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.OwnerRecoveryComplete {
		t.Fatal("owner 恢复完整时应上报 true")
	}
	if res.Items[0].Exec != store.CallExecDone || res.Items[0].Phase == store.CallPhaseOrphaned {
		t.Fatalf("应按 owner 答案上报 exec=done: %+v", res.Items[0])
	}
	if res.Items[1].Exec != store.CallExecFailed {
		t.Fatalf("应按 owner 答案上报 exec=failed: %+v", res.Items[1])
	}
	// 每个 service 各问一次，且两维在本地账本里保持原值（只读对账）。
	sent := map[string]bool{}
	for _, svc := range fake.lastStatusServices() {
		sent[svc] = true
	}
	if !sent["shell"] || !sent["python"] {
		t.Fatalf("对账应按 call 的 service 分批询问，实际 = %v", fake.lastStatusServices())
	}
	for id, want := range map[string]string{shellID: store.CallExecRunning, pyRes.CallID: store.CallExecRunning} {
		if row := mustCallRow(t, st, id); row.Exec != want {
			t.Fatalf("对账不应改本地账本（%s）: %+v", id, row)
		}
	}
}

// ---------- nil 客户端：保持诚实边界 ----------

func TestOwnerContractNilClientKeepsHonestBehavior(t *testing.T) {
	s, st := newOwnerContractServer(t, nil)
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	dispatchCallsTestCall(t, st, id)

	// pause / resume：没有 owner 可确认 → unavailable，且不改状态。
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-nil-pause", Op: "pause"}); !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("nil client 时 pause 应 unavailable: %v", err)
	}
	if row := mustCallRow(t, st, id); row.Exec != store.CallExecRunning || row.Observe != store.CallObserveWaiting {
		t.Fatalf("nil client 的 pause 不得改状态: %+v", row)
	}
	markCallsTestPaused(t, st, id)
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-nil-resume", Op: "resume"}); !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("nil client 时 resume 应 unavailable: %v", err)
	}
	if row := mustCallRow(t, st, id); row.Exec != store.CallExecPaused {
		t.Fatalf("nil client 的 resume 不得改状态: %+v", row)
	}

	// call_status：owner_recovery_complete 恒 false；查不到的 id 不判 orphaned。
	res, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: []string{id, "missing-call"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.OwnerRecoveryComplete {
		t.Fatal("nil client 时 owner_recovery_complete 必须为 false")
	}
	if res.Items[1].Exec != "" || res.Items[1].Phase != "" {
		t.Fatalf("nil client 时未知 id 不得被判定: %+v", res.Items[1])
	}

	// cancel：仍本地执行（不可暂停 → unknown + remote_may_continue）。
	id2 := openCallsTestCall(t, s, "w1", store.CallModeOnce, false)
	dispatchCallsTestCall(t, st, id2)
	cancelRes, err := controlCalls(t, s, protocol.CallControlPayload{ID: id2, ControlID: "ctl-nil-cancel", Op: "cancel"})
	if err != nil {
		t.Fatalf("nil client 时 cancel 应本地执行: %v", err)
	}
	if cancelRes.Observe != store.CallObserveClosed || cancelRes.Exec != store.CallExecUnknown || cancelRes.Reason != "remote_may_continue" {
		t.Fatalf("nil client 的 cancel 结果不对: %+v", cancelRes)
	}
	if row := mustCallRow(t, st, id2); !row.RemoteMayContinue {
		t.Fatalf("nil client 的不可暂停 cancel 应标 remote_may_continue: %+v", row)
	}
}
