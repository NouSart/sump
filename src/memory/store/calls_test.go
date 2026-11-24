package store

// 调用账本 / 图片索引（S-001 §4.5 / §4.7 / §5.1）的存储层证据：
// 初始两维、非法转移、control 幂等、常驻过滤、对账候选、图片索引覆盖写与隔离。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/spv2"
)

func openCallsTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "calls.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// ensureCallsWork 登记一条工作 work（owner 可不同，用来验证常驻目录的 owner 隔离）。
func ensureCallsWork(t *testing.T, st *Store, id, owner string) {
	t.Helper()
	if _, err := st.EnsureWork(context.Background(), spv2.Work{
		ID: id, Root: id, Class: spv2.ClassRequest,
		Owner: spv2.HumanRef(owner), Agent: spv2.AgentRef("reasoner"),
		Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatalf("登记 work %s: %v", id, err)
	}
}

func createCallsTestCall(t *testing.T, st *Store, work, service, mode string, pausable bool) string {
	t.Helper()
	id, err := st.CreateCall(context.Background(), work, "", "", "inst-1", service, "run", mode, pausable, 60000, 300000)
	if err != nil {
		t.Fatalf("CreateCall(%s): %v", work, err)
	}
	return id
}

func TestCreateCallInitialState(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")

	id, err := st.CreateCall(ctx, "w1", "", "alice", "inst-1", "shell", "run", CallModeResident, true, 60000, 300000)
	if err != nil {
		t.Fatal(err)
	}
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Observe != CallObserveWaiting || row.Exec != CallExecCreated || row.Phase != "" {
		t.Fatalf("初始两维不对: observe=%q exec=%q phase=%q（期望 waiting/created/NULL）", row.Observe, row.Exec, row.Phase)
	}
	if row.Work != "w1" || row.Service != "shell" || row.Mode != CallModeResident || !row.Pausable {
		t.Fatalf("call 行字段不对: %+v", row)
	}
	if row.TTLMS != 60000 || row.ExecDeadlineMS != 300000 || row.RemoteItemID != "" || row.RemoteMayContinue {
		t.Fatalf("ttl/deadline/remote 字段不对: %+v", row)
	}
	// owner 从 works 联表补齐（calls 表本身没有 owner 列）。
	if row.Owner != "alice" || row.Task != "" {
		t.Fatalf("联表字段不对: owner=%q task=%q", row.Owner, row.Task)
	}

	// work 不存在必须明确失败，而不是写一条无主调用。
	if _, err := st.CreateCall(ctx, "nope", "", "", "", "shell", "run", CallModeOnce, false, 0, 0); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("缺 work 应 ErrWorkNotFound: %v", err)
	}
	// mode 非法。
	if _, err := st.CreateCall(ctx, "w1", "", "alice", "", "shell", "run", "forever", false, 0, 0); err == nil {
		t.Fatal("非法 mode 应报错")
	}
}

func TestCallStateTransitions(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")
	id := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)

	// created → done 非法（没执行过不能有结果）。
	err := st.SetCallState(ctx, id, CallObserveWaiting, CallExecDone, CallPhaseDone, "")
	if !errors.Is(err, ErrCallConflict) {
		t.Fatalf("created→done 应 ErrCallConflict: %v", err)
	}
	if row, _ := st.GetCall(ctx, id); row.Exec != CallExecCreated {
		t.Fatalf("非法转移不应落库: exec=%q", row.Exec)
	}

	// 合法路径：created → running → paused → running → cancelled。
	for _, step := range []struct {
		observe, exec, phase string
	}{
		{CallObserveWaiting, CallExecRunning, ""},
		{CallObserveWaiting, CallExecPaused, ""},
		{CallObserveWaiting, CallExecRunning, ""},
		{CallObserveClosed, CallExecCancelled, CallPhaseCancelled},
	} {
		if err := st.SetCallState(ctx, id, step.observe, step.exec, step.phase, ""); err != nil {
			t.Fatalf("合法转移 %s/%s 失败: %v", step.observe, step.exec, err)
		}
	}

	// cancelled 是终态：不能再 running。
	if err := st.SetCallState(ctx, id, CallObserveWaiting, CallExecRunning, "", ""); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("cancelled→running 应 ErrCallConflict: %v", err)
	}

	// done 是终态：不能再 running；重复写同一状态是幂等的。
	id2 := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)
	if err := st.SetCallState(ctx, id2, CallObserveWaiting, CallExecRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallState(ctx, id2, CallObserveClosed, CallExecDone, CallPhaseDone, "done"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallState(ctx, id2, CallObserveWaiting, CallExecRunning, "", ""); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("done→running 应 ErrCallConflict: %v", err)
	}
	if err := st.SetCallState(ctx, id2, CallObserveClosed, CallExecDone, CallPhaseDone, "done again"); err != nil {
		t.Fatalf("同状态重写应允许: %v", err)
	}

	// closed 的观察维不允许重开。
	if err := st.SetCallState(ctx, id2, CallObserveWaiting, CallExecDone, CallPhaseDone, ""); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("closed→waiting 应 ErrCallConflict: %v", err)
	}

	// 不存在的 call。
	if err := st.SetCallState(ctx, "nope", CallObserveWaiting, CallExecRunning, "", ""); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("不存在的 call 应 ErrCallNotFound: %v", err)
	}
}

func TestControlIdempotent(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")
	id := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)

	raw, deduped, err := st.PutControl(ctx, "c1", id, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if deduped || raw != "" {
		t.Fatalf("首次 PutControl 应 (\"\", false)，得到 (%q, %v)", raw, deduped)
	}
	if err := st.SetControlResult(ctx, "c1", CallObserveClosed, CallExecCancelled, "cancelled"); err != nil {
		t.Fatal(err)
	}

	// 同 control_id 重放：返回首次结果，不再插入。
	raw2, deduped2, err := st.PutControl(ctx, "c1", id, "cancel")
	if err != nil {
		t.Fatal(err)
	}
	if !deduped2 {
		t.Fatal("同 control_id 重放应 deduped=true")
	}
	first, err := DecodeControlResult(raw2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Observe != CallObserveClosed || first.Exec != CallExecCancelled || first.Reason != "cancelled" {
		t.Fatalf("重放结果与首次不一致: %+v", first)
	}

	// 同 control_id 换 op = 冲突（不能拿一个 control 干两件事）。
	if _, _, err := st.PutControl(ctx, "c1", id, "pause"); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("同 control_id 换 op 应 ErrCallConflict: %v", err)
	}

	row, ok, err := st.GetControl(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("GetControl = (%+v, %v, %v)", row, ok, err)
	}
	if row.CallID != id || row.Op != "cancel" || row.Result == "" || row.Observe != CallObserveClosed {
		t.Fatalf("control 行不对: %+v", row)
	}

	// call 不存在。
	if _, _, err := st.PutControl(ctx, "c2", "nope", "cancel"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("不存在的 call 应 ErrCallNotFound: %v", err)
	}
}

func TestSetCallRemoteBackfill(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")
	id := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)

	if err := st.SetCallRemote(ctx, id, "remote-1"); err != nil {
		t.Fatal(err)
	}
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.RemoteItemID != "remote-1" {
		t.Fatalf("remote_item_id = %q, want remote-1", row.RemoteItemID)
	}

	// 同一 service/work 下 remote_item_id 唯一（部分唯一索引）。
	id2 := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, false)
	if err := st.SetCallRemote(ctx, id2, "remote-1"); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("remote_item_id 撞键应 ErrCallConflict: %v", err)
	}
	if err := st.SetCallRemote(ctx, "nope", "remote-2"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("不存在的 call 应 ErrCallNotFound: %v", err)
	}
}

func TestListResidentFiltersOwnerAndMode(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w-alice", "alice")
	ensureCallsWork(t, st, "w-bob", "bob")

	a1 := createCallsTestCall(t, st, "w-alice", "shell", CallModeResident, true)
	time.Sleep(time.Millisecond) // 让 created_at 明确分先后
	a2 := createCallsTestCall(t, st, "w-alice", "shell", CallModeResident, true)
	_ = createCallsTestCall(t, st, "w-alice", "shell", CallModeOnce, true) // once 不进常驻目录
	_ = createCallsTestCall(t, st, "w-bob", "shell", CallModeResident, true)

	rows, err := st.ListResident(ctx, "alice", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("alice 常驻调用 = %d, want 2（once 与 bob 的 resident 都不应出现）", len(rows))
	}
	if rows[0].ID != a2 || rows[1].ID != a1 {
		t.Fatalf("应按 created_at 倒序: got [%s, %s]，want [%s, %s]", rows[0].ID, rows[1].ID, a2, a1)
	}
	for _, row := range rows {
		if row.Mode != CallModeResident || row.Owner != "alice" {
			t.Fatalf("常驻行混入非目标: %+v", row)
		}
	}

	// 游标分页：拿上一页最后一条 id 继续。
	page1, err := st.ListResident(ctx, "alice", 1, "")
	if err != nil || len(page1) != 1 || page1[0].ID != a2 {
		t.Fatalf("第一页 = %+v, err=%v", page1, err)
	}
	page2, err := st.ListResident(ctx, "alice", 1, page1[0].ID)
	if err != nil || len(page2) != 1 || page2[0].ID != a1 {
		t.Fatalf("第二页 = %+v, err=%v", page2, err)
	}
}

func TestListCallsForReconcile(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")

	open := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)
	closed := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)
	if err := st.SetCallState(ctx, closed, CallObserveWaiting, CallExecRunning, "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallState(ctx, closed, CallObserveClosed, CallExecDone, CallPhaseDone, "done"); err != nil {
		t.Fatal(err)
	}
	// 已关闭但 exec=unknown 的调用仍要参与对账。
	unknown := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, false)
	if err := st.SetCallState(ctx, unknown, CallObserveClosed, CallExecUnknown, "", "ack_lost"); err != nil {
		t.Fatal(err)
	}

	rows, err := st.ListCallsForReconcile(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, row := range rows {
		got[row.ID] = true
	}
	if !got[open] {
		t.Fatal("未关闭的 call 应在对账候选里")
	}
	if !got[unknown] {
		t.Fatal("exec=unknown 的 call 应在对账候选里")
	}
	if got[closed] {
		t.Fatal("已关闭且 done 的 call 不应进对账候选")
	}
}

func TestCreateCallTaskAndOwnerValidation(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")
	task, err := st.OpenTask("conv-1", "测试任务")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkWorkTask(ctx, "w1", task.ID); err != nil {
		t.Fatal(err)
	}
	owner, gotTask, err := st.WorkOwnerTask(ctx, "w1")
	if err != nil || owner != "alice" || gotTask != task.ID {
		t.Fatalf("WorkOwnerTask = (%q, %q, %v)", owner, gotTask, err)
	}
	if _, err := st.CreateCall(ctx, "w1", task.ID, "alice", "", "shell", "run", CallModeOnce, false, 0, 0); err != nil {
		t.Fatalf("task/owner 匹配时应成功: %v", err)
	}
	// task / owner 与 works 不一致必须拒绝，避免把调用记到错的 work 上。
	if _, err := st.CreateCall(ctx, "w1", "other-task", "alice", "", "shell", "run", CallModeOnce, false, 0, 0); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("task 不一致应 ErrCallConflict: %v", err)
	}
	if _, err := st.CreateCall(ctx, "w1", task.ID, "bob", "", "shell", "run", CallModeOnce, false, 0, 0); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("owner 不一致应 ErrCallConflict: %v", err)
	}
	// 关联不存在的 task。
	if err := st.LinkWorkTask(ctx, "w1", "nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("关联不存在的 task 应 ErrTaskNotFound: %v", err)
	}
}

func TestImageIndexUpsertAndGet(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()

	if err := st.UpsertImageIndex(ctx, "alice", "conv-1", "img-1", "第一眼", ImageStatusStored); err != nil {
		t.Fatal(err)
	}
	first, err := st.GetImageIndex(ctx, "alice", "conv-1", "img-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Gist != "第一眼" || first.Status != ImageStatusStored || first.FirstSeenAt == "" {
		t.Fatalf("首次写入不对: %+v", first)
	}

	time.Sleep(time.Millisecond)
	if err := st.UpsertImageIndex(ctx, "alice", "conv-1", "img-1", "第二眼", ImageStatusLost); err != nil {
		t.Fatal(err)
	}
	second, err := st.GetImageIndex(ctx, "alice", "conv-1", "img-1")
	if err != nil {
		t.Fatal(err)
	}
	if second.Gist != "第二眼" || second.Status != ImageStatusLost {
		t.Fatalf("覆盖写没生效: %+v", second)
	}
	if second.FirstSeenAt != first.FirstSeenAt {
		t.Fatalf("first_seen_at 应保留首次值: %q vs %q", first.FirstSeenAt, second.FirstSeenAt)
	}
	if second.LastViewedAt == first.LastViewedAt {
		t.Fatalf("last_viewed_at 应更新: %q", second.LastViewedAt)
	}

	// owner / 会话隔离：换成 bob 或别的会话都读不到。
	if _, err := st.GetImageIndex(ctx, "bob", "conv-1", "img-1"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("跨 owner 应 ErrImageNotFound: %v", err)
	}
	if _, err := st.GetImageIndex(ctx, "alice", "conv-2", "img-1"); !errors.Is(err, ErrImageNotFound) {
		t.Fatalf("跨会话应 ErrImageNotFound: %v", err)
	}
	// status 只允许 stored / lost。
	if err := st.UpsertImageIndex(ctx, "alice", "conv-1", "img-1", "x", "weird"); err == nil {
		t.Fatal("非法 status 应报错")
	}
}
