package tests

import (
	"testing"
	"time"

	"github.com/NouSart/sump/batch/server"
	"github.com/NouSart/sump/protocol"
)

// TestRunAcksImmediatelyWithItemID：受理即回执，句柄字段必须是 item_id（reasoner 只认它）。
func TestRunAcksImmediatelyWithItemID(t *testing.T) {
	f := startFixture(t, nil)

	start := time.Now()
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "slow",
		Items: []protocol.BatchItem{{Input: mustRaw(map[string]any{"ms": 800})}},
	})
	if time.Since(start) > 500*time.Millisecond {
		t.Fatalf("受理回执不该等执行完成，耗时 %s", time.Since(start))
	}
	if res.ItemID == "" || res.Total != 1 || res.Phase != protocol.BatchPhaseAccepted {
		t.Fatalf("回执不符: %+v", res)
	}
	if res.Kind != protocol.BatchKindOnce {
		t.Fatalf("Kind 预设应为 once，实际 %q", res.Kind)
	}

	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 6*time.Second)
	if done.Ok != 1 || done.Pending != 0 {
		t.Fatalf("终态计数不符: %+v", done)
	}
	final := f.next(t, 3*time.Second)
	if !final.Final || final.ItemID != res.ItemID {
		t.Fatalf("终态补报不符: %+v", final)
	}
}

// TestManyItemsAllSucceed：50 项全成功。
func TestManyItemsAllSucceed(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(50), Concurrency: 16})
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 15*time.Second)
	if done.Ok != 50 || done.Failed != 0 {
		t.Fatalf("计数不符: %+v", done)
	}
	if got := f.target.count("ok"); got != 50 {
		t.Fatalf("目标只被调了 %d 次", got)
	}
	final := f.next(t, 3*time.Second)
	if !final.Final {
		t.Fatalf("终态补报不符: %+v", final)
	}
}

// TestFailureGrouping：3 项同错 → 汇总里 1 组，不是 3 行。
func TestFailureGrouping(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "fail", Items: items(3)})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseFailed, 8*time.Second)

	var final protocol.AsyncResultPayload
	for i := 0; i < 2; i++ {
		p := f.next(t, 3*time.Second)
		if p.Final {
			final = p
		}
	}
	if final.Text == "" {
		t.Fatal("没收到终态补报")
	}
	if got := countOccurrences(final.Text, "目标说这个不行"); got != 1 {
		t.Fatalf("同一原因应归成 1 组，实际出现 %d 次：\n%s", got, final.Text)
	}
	if !contains(final.Text, "（3 项）") {
		t.Fatalf("汇总应写明 3 项：\n%s", final.Text)
	}
}

// TestFirstFailureNotifiesOnce：串行 5 项全失败 → 首次失败通知只发一次。
func TestFirstFailureNotifiesOnce(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "fail", Items: items(5), Concurrency: 1})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseFailed, 12*time.Second)

	first := f.next(t, 3*time.Second)
	if first.Final || first.Phase != protocol.BatchPhaseProgress {
		t.Fatalf("第一条应为首次失败通知，实际 %+v", first)
	}
	if !contains(first.Text, "失败 1") {
		t.Fatalf("首次失败通知应带计数快照：\n%s", first.Text)
	}
	second := f.next(t, 3*time.Second)
	if !second.Final {
		t.Fatalf("第二条应为终态，实际 %+v", second)
	}
	f.quiet(t, 600*time.Millisecond)
}

// TestWakeAllSendsOnePush：wake=all 只推一条终态。
func TestWakeAllSendsOnePush(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(6)})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 8*time.Second)
	p := f.next(t, 3*time.Second)
	if !p.Final {
		t.Fatalf("all 档只该有一条终态，实际 %+v", p)
	}
	f.quiet(t, 500*time.Millisecond)
}

// TestWakeKeySendsTwoPushes：key 唤醒 + 终态 = 两条。
func TestWakeKeySendsTwoPushes(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "slow",
		Items: []protocol.BatchItem{
			{Input: mustRaw(map[string]any{"ms": 20}), Key: true},
			{Input: mustRaw(map[string]any{"ms": 20}), Key: true},
			{Input: mustRaw(map[string]any{"ms": 500})},
			{Input: mustRaw(map[string]any{"ms": 500})},
		},
		Concurrency: 2,
		Wake:        protocol.BatchWakePolicy{Mode: protocol.BatchWakeKey},
	})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 10*time.Second)

	first := f.next(t, 3*time.Second)
	second := f.next(t, 3*time.Second)
	if first.Final {
		t.Fatalf("第一条应是 key 唤醒（非终态），实际 %+v", first)
	}
	if !second.Final {
		t.Fatalf("第二条应是终态，实际 %+v", second)
	}
	f.quiet(t, 400*time.Millisecond)
}

// TestWakeKeyRequiresKeyItems：mode=key 但没有 key 项 → 拒绝。
func TestWakeKeyRequiresKeyItems(t *testing.T) {
	f := startFixture(t, nil)
	_, err := f.callRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok", Items: items(2),
		Wake: protocol.BatchWakePolicy{Mode: protocol.BatchWakeKey},
	})
	if err == nil || !contains(err.Error(), "BATCH_WAKE") {
		t.Fatalf("应报 BATCH_WAKE，实际 %v", err)
	}
}

// TestStopOnError：失败即停，其余转 skipped。
func TestStopOnError(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "fail", Items: items(5), Concurrency: 1,
		OnError: protocol.BatchOnErrorStop,
	})
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseFailed, 12*time.Second)
	if done.Failed != 1 || done.Skipped != 4 {
		t.Fatalf("失败即停后应为 1 失败 4 跳过，实际 %+v", done)
	}
}

// TestCancelSkipsUnstarted：取消后未开始的项作废，在飞的跑完。
func TestCancelSkipsUnstarted(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "slow",
		Items: []protocol.BatchItem{
			{Input: mustRaw(map[string]any{"ms": 300})},
			{Input: mustRaw(map[string]any{"ms": 300})},
			{Input: mustRaw(map[string]any{"ms": 300})},
			{Input: mustRaw(map[string]any{"ms": 300})},
		},
		Concurrency: 1,
	})
	time.Sleep(120 * time.Millisecond)
	ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpCancel, ItemID: res.ItemID})
	if ctl.Affected != 1 {
		t.Fatalf("取消应有 1 个任务受影响，实际 %+v", ctl)
	}
	// 取消是终态，但在飞的项跑完才真正落地。
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseCancelled, 10*time.Second)
	if done.Ok < 1 {
		t.Fatalf("在飞的那项应该跑完，实际 %+v", done)
	}
	if done.Skipped < 2 {
		t.Fatalf("未开始的项应转为跳过，实际 %+v", done)
	}
	if done.Ok+done.Skipped != 4 {
		t.Fatalf("跑完 + 跳过应等于总项数，实际 %+v", done)
	}
}

// TestPauseResumesWithoutRerun：暂停可恢复，已完成的项不重跑。
func TestPauseResumesWithoutRerun(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 6)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 80})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	time.Sleep(120 * time.Millisecond)
	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID})
	paused := f.status(t, res.ItemID)
	if paused.Phase != protocol.BatchPhasePaused {
		t.Fatalf("应转 paused，实际 %s", paused.Phase)
	}
	// 暂停不打断在飞的项：先等它跑完，再看是否真的静止。
	time.Sleep(400 * time.Millisecond)
	settled := f.status(t, res.ItemID)
	time.Sleep(500 * time.Millisecond)
	still := f.status(t, res.ItemID)
	if still.Ok != settled.Ok {
		t.Fatalf("暂停期间不该再有项完成: %d → %d", settled.Ok, still.Ok)
	}
	if still.Ok >= 6 {
		t.Fatalf("暂停时不该已经跑完（说明暂停没生效）: %+v", still)
	}

	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, ItemID: res.ItemID})
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 12*time.Second)
	if done.Ok != 6 {
		t.Fatalf("恢复后应跑完全部 6 项，实际 %+v", done)
	}
	if got := f.target.count("slow"); got != 6 {
		t.Fatalf("已完成的项不该重跑：目标被调了 %d 次（应为 6）", got)
	}
}

// TestNestedRejected：不允许批量调用 batch 自己。
func TestNestedRejected(t *testing.T) {
	f := startFixture(t, nil)
	_, err := f.callRun(t, protocol.BatchRunPayload{Service: "batch", Action: "status", Items: items(1)})
	if err == nil || !contains(err.Error(), "BATCH_NESTED") {
		t.Fatalf("应报 BATCH_NESTED，实际 %v", err)
	}
}

// TestNotBatchableRejected：目标动作没声明 Batchable → 拒绝。
func TestNotBatchableRejected(t *testing.T) {
	f := startFixture(t, nil)
	_, err := f.callRun(t, protocol.BatchRunPayload{Service: "worker", Action: "plain", Items: items(1)})
	if err == nil || !contains(err.Error(), "BATCH_NOT_BATCHABLE") {
		t.Fatalf("应报 BATCH_NOT_BATCHABLE，实际 %v", err)
	}
}

// TestItemsLimit：超过 max_items → 拒绝。
func TestItemsLimit(t *testing.T) {
	f := startFixture(t, func(c *server.Config) { c.MaxItems = 3 })
	_, err := f.callRun(t, protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(4)})
	if err == nil || !contains(err.Error(), "BATCH_ITEMS") {
		t.Fatalf("应报 BATCH_ITEMS，实际 %v", err)
	}
}

// TestGlobalBusyRejectsSync：全局槽位满 → 同步拒绝，不排队。
func TestGlobalBusyRejectsSync(t *testing.T) {
	f := startFixture(t, func(c *server.Config) { c.MaxInflight = 2 })
	first := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "slow",
		Items: []protocol.BatchItem{
			{Input: mustRaw(map[string]any{"ms": 1000})},
			{Input: mustRaw(map[string]any{"ms": 1000})},
		},
		Concurrency: 2,
	})
	time.Sleep(150 * time.Millisecond)
	if s := f.status(t, first.ItemID); s.Phase != protocol.BatchPhaseRunning {
		t.Fatalf("第一个任务应还在跑，实际 %s", s.Phase)
	}

	start := time.Now()
	_, err := f.callRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok", Items: items(1), Concurrency: 1,
	})
	if err == nil || !contains(err.Error(), "BATCH_BUSY") {
		t.Fatalf("应报 BATCH_BUSY，实际 %v", err)
	}
	if time.Since(start) > 600*time.Millisecond {
		t.Fatalf("拒绝应当同步返回，耗时 %s", time.Since(start))
	}
	f.waitPhase(t, first.ItemID, protocol.BatchPhaseDone, 10*time.Second)
}

// TestListAndResult：队列过滤与明细分页。
func TestListAndResult(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(5)})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 8*time.Second)

	list := mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{})
	if len(list.Jobs) == 0 || list.Counts.Done == 0 {
		t.Fatalf("队列应有已结束任务: %+v", list)
	}
	failed := mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{Phase: protocol.BatchPhaseFailed})
	if len(failed.Jobs) != 0 {
		t.Fatalf("没有失败任务，过滤后应为空: %+v", failed.Jobs)
	}

	page := mustCall[protocol.BatchResultResult](f, t, "result", protocol.BatchResultPayload{ItemID: res.ItemID, Limit: 2})
	if page.Matched != 5 || len(page.Items) != 2 || page.Items[0].Seq != 0 {
		t.Fatalf("分页不符: %+v", page)
	}
	next := mustCall[protocol.BatchResultResult](f, t, "result", protocol.BatchResultPayload{ItemID: res.ItemID, Limit: 2, Offset: 2})
	if len(next.Items) != 2 || next.Items[0].Seq != 2 {
		t.Fatalf("第二页不符: %+v", next)
	}
}
