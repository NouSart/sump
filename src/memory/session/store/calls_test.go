package store

// 调用账本（S-001 §4.5 / §5.1）的存储层证据：初始两维、非法转移、control 幂等、
// remote 回填唯一、常驻过滤与分页、works 联表字段。
//
// 与源 af6cfc8:src/memory/store/calls_test.go 的差异（R-023 P3）：
//   - 图片索引用例不搬：image_index 不在本架构（D7）；
//   - 对账候选（ListCallsForReconcile）不搬：不建周期 reconciler（D7），没有消费者；
//   - CreateCall 去掉 task / owner / instance 入参：task 与 owner 由 works 联表解析，
//     instance 在 schema 与协议里都没有落点；「调用方声明与账本不一致」这个入口一并消失。

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
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

// ensureCallsWork（登记 work 的脚手架）在 record_v2_test.go，同包复用。

func createCallsTestCall(t *testing.T, st *Store, work, service, mode string, pausable bool) string {
	t.Helper()
	id, err := st.CreateCall(context.Background(), work, service, "run", mode, pausable, 60000, 300000)
	if err != nil {
		t.Fatalf("CreateCall(%s): %v", work, err)
	}
	return id
}

func TestCreateCallInitialState(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")

	id, err := st.CreateCall(ctx, "w1", "shell", "run", CallModeResident, true, 60000, 300000)
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
	if _, err := st.CreateCall(ctx, "nope", "shell", "run", CallModeOnce, false, 0, 0); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("缺 work 应 ErrWorkNotFound: %v", err)
	}
	// 参数校验：work / service 必填，时限不能为负，mode 只允许 once / resident。
	if _, err := st.CreateCall(ctx, "", "shell", "run", CallModeOnce, false, 0, 0); err == nil {
		t.Fatal("缺 work 应报错")
	}
	if _, err := st.CreateCall(ctx, "w1", "", "run", CallModeOnce, false, 0, 0); err == nil {
		t.Fatal("缺 service 应报错")
	}
	if _, err := st.CreateCall(ctx, "w1", "shell", "run", CallModeOnce, false, -1, 0); err == nil {
		t.Fatal("负 ttl_ms 应报错")
	}
	if _, err := st.CreateCall(ctx, "w1", "shell", "run", "forever", false, 0, 0); err == nil {
		t.Fatal("非法 mode 应报错")
	}
	// 缺 mode 按 once 落库（调用方省略 mode 是正常路径）。
	id2, err := st.CreateCall(ctx, "w1", "shell", "", "", false, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if row2, err := st.GetCall(ctx, id2); err != nil || row2.Mode != CallModeOnce {
		t.Fatalf("缺省 mode 应为 once: %+v, err=%v", row2, err)
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

	// 不存在的 call / 非法取值。
	if err := st.SetCallState(ctx, "nope", CallObserveWaiting, CallExecRunning, "", ""); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("不存在的 call 应 ErrCallNotFound: %v", err)
	}
	if err := st.SetCallState(ctx, id2, "someday", CallExecDone, CallPhaseDone, ""); err == nil {
		t.Fatal("非法 observe 取值应报错")
	}
	if err := st.SetCallState(ctx, id2, CallObserveClosed, CallExecDone, "weird", ""); err == nil {
		t.Fatal("非法 phase 取值应报错")
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
	// 首次结果还没落库时重放：宁可 conflict，也不返回假结果。
	if _, _, err := st.PutControl(ctx, "c1", id, "cancel"); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("首次结果未落库的重放应 ErrCallConflict: %v", err)
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
	// 同 control_id 换 call = 冲突。
	id2 := createCallsTestCall(t, st, "w1", "shell", CallModeOnce, true)
	if _, _, err := st.PutControl(ctx, "c1", id2, "cancel"); !errors.Is(err, ErrCallConflict) {
		t.Fatalf("同 control_id 换 call 应 ErrCallConflict: %v", err)
	}

	row, ok, err := st.GetControl(ctx, "c1")
	if err != nil || !ok {
		t.Fatalf("GetControl = (%+v, %v, %v)", row, ok, err)
	}
	if row.CallID != id || row.Op != "cancel" || row.Result == "" || row.Observe != CallObserveClosed {
		t.Fatalf("control 行不对: %+v", row)
	}
	if _, ok, err := st.GetControl(ctx, "never"); ok || err != nil {
		t.Fatalf("未出现过的 control 应 (false, nil): ok=%v err=%v", ok, err)
	}

	// call 不存在。
	if _, _, err := st.PutControl(ctx, "c2", "nope", "cancel"); !errors.Is(err, ErrCallNotFound) {
		t.Fatalf("不存在的 call 应 ErrCallNotFound: %v", err)
	}
	if err := st.SetControlResult(ctx, "never", "", "", ""); !errors.Is(err, ErrControlNotFound) {
		t.Fatalf("不存在的 control 应 ErrControlNotFound: %v", err)
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
	if err := st.SetCallRemote(ctx, id2, "  "); err == nil {
		t.Fatal("空 remote_item_id 应报错")
	}

	// remote_may_continue 标记（取消未获确认时的落点）。
	if err := st.SetCallRemoteMayContinue(ctx, id2, true); err != nil {
		t.Fatal(err)
	}
	if row, _ := st.GetCall(ctx, id2); !row.RemoteMayContinue {
		t.Fatal("remote_may_continue 未落库")
	}
	if err := st.SetCallRemoteMayContinue(ctx, "nope", true); !errors.Is(err, ErrCallNotFound) {
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

	// owner 必填：空 owner 是参数错误，不是「所有人的常驻调用」。
	if _, err := st.ListResident(ctx, " ", 10, ""); err == nil {
		t.Fatal("空 owner 应报错")
	}
}

// TestCreateCallResolvesWorkJoin 钉子：task / owner 只来自 works 行（不是调用方声明）。
func TestCreateCallResolvesWorkJoin(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")
	task, err := st.OpenTask("conv-1", "调用测试任务")
	if err != nil {
		t.Fatal(err)
	}
	// works.task 的写入方是会话生命周期（work_open → LinkWorkTask），这里走同一个入口。
	if err := st.LinkWorkTask(ctx, "w1", task.ID); err != nil {
		t.Fatal(err)
	}

	id := createCallsTestCall(t, st, "w1", "shell", CallModeResident, true)
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Owner != "alice" || row.Task != task.ID {
		t.Fatalf("GetCall 联表字段不对: owner=%q task=%q（want alice / %s）", row.Owner, row.Task, task.ID)
	}
	rows, err := st.ListResident(ctx, "alice", 10, "")
	if err != nil || len(rows) != 1 || rows[0].Task != task.ID {
		t.Fatalf("ListResident 联表字段不对: %+v, err=%v", rows, err)
	}
}

// TestLinkWorkTask 钉住 work → task 归属的四个分支（R-023 P4.5）：
// 成功 / 幂等 / 已挂别的 task 冲突 / task 不存在；另外守住「work 不存在」与空参数。
func TestLinkWorkTask(t *testing.T) {
	st := openCallsTestStore(t)
	ctx := context.Background()
	ensureCallsWork(t, st, "w1", "alice")
	task, err := st.OpenTask("conv-1", "归属任务")
	if err != nil {
		t.Fatal(err)
	}
	other, err := st.OpenTask("conv-2", "另一个任务")
	if err != nil {
		t.Fatal(err)
	}

	// 成功：works.task 立即指向任务。
	if err := st.LinkWorkTask(ctx, "w1", task.ID); err != nil {
		t.Fatalf("首次关联失败: %v", err)
	}
	if got, err := st.TaskIDOfWork(ctx, "w1"); err != nil || got != task.ID {
		t.Fatalf("TaskIDOfWork = (%q, %v)，期望 %q", got, err, task.ID)
	}

	// 幂等：同一 work + 同一 task 重复关联成功，值不变。
	if err := st.LinkWorkTask(ctx, "w1", task.ID); err != nil {
		t.Fatalf("重复关联同一任务应幂等: %v", err)
	}
	if got, _ := st.TaskIDOfWork(ctx, "w1"); got != task.ID {
		t.Fatalf("幂等重放不应改归属: %q", got)
	}

	// 冲突：已挂别的 task 必须 ErrCallConflict，且原归属不变。
	err = st.LinkWorkTask(ctx, "w1", other.ID)
	if !errors.Is(err, ErrCallConflict) {
		t.Fatalf("改挂别的任务应 ErrCallConflict: %v", err)
	}
	if got, _ := st.TaskIDOfWork(ctx, "w1"); got != task.ID {
		t.Fatalf("冲突失败后归属不应变: %q", got)
	}

	// task 不存在：ErrTaskNotFound，且原归属不变。
	if err := st.LinkWorkTask(ctx, "w1", "t-nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("关联不存在的 task 应 ErrTaskNotFound: %v", err)
	}
	if got, _ := st.TaskIDOfWork(ctx, "w1"); got != task.ID {
		t.Fatalf("失败后归属不应变: %q", got)
	}
	// 不存在的 task 优先于「已关联别的 task」：不能拿冲突掩盖 task 不存在。
	ensureCallsWork(t, st, "w2", "alice")
	if err := st.LinkWorkTask(ctx, "w2", task.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.LinkWorkTask(ctx, "w2", "t-nope"); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("已有归属时不存在的 task 仍应 ErrTaskNotFound: %v", err)
	}

	// work 不存在 / 空参数：显式哨兵，不写任何行。
	if err := st.LinkWorkTask(ctx, "w-nope", task.ID); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("work 不存在应 ErrWorkNotFound: %v", err)
	}
	if err := st.LinkWorkTask(ctx, " ", task.ID); !errors.Is(err, ErrWorkNotFound) {
		t.Fatalf("空 work 应 ErrWorkNotFound: %v", err)
	}
	if err := st.LinkWorkTask(ctx, "w1", " "); !errors.Is(err, ErrTaskNotFound) {
		t.Fatalf("空 task 应 ErrTaskNotFound: %v", err)
	}
}
