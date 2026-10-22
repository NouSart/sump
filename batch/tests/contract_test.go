package tests

import (
	"context"
	"encoding/json"
	"log"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/batch/server"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// collect 收补报直到终态（或超时失败）。
func collect(t *testing.T, f *fixture, within time.Duration) []protocol.AsyncResultPayload {
	t.Helper()
	var out []protocol.AsyncResultPayload
	limit := time.After(within)
	for {
		select {
		case p := <-f.reports:
			out = append(out, p)
			if p.Final {
				return out
			}
		case <-limit:
			t.Fatalf("%s 内没等到终态补报（已收 %d 条）", within, len(out))
		}
	}
}

// TestFailedItemVisibleInResult：失败项必须真的落进账本。
//
// 这里踩过坑：items.preview 是 NOT NULL，失败项没有摘要，传 nil 会被 SQLite 拒，
// 于是"失败"只活在内存里——任务落账了，明细却永远停在 running。
func TestFailedItemVisibleInResult(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "fail", Items: items(2), Concurrency: 2})
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseFailed, 10*time.Second)
	if done.Failed != 2 {
		t.Fatalf("应有 2 项失败，实际 %+v", done)
	}
	page := mustCall[protocol.BatchResultResult](f, t, "result",
		protocol.BatchResultPayload{ItemID: res.ItemID, Status: protocol.BatchItemFailed})
	if page.Matched != 2 || len(page.Items) != 2 {
		t.Fatalf("明细里应有 2 个失败项，实际 matched=%d len=%d", page.Matched, len(page.Items))
	}
	for _, it := range page.Items {
		if it.Status != protocol.BatchItemFailed || it.Error == "" {
			t.Fatalf("失败项应带状态与原因，实际 %+v", it)
		}
	}
	if stuck := mustCall[protocol.BatchResultResult](f, t, "result",
		protocol.BatchResultPayload{ItemID: res.ItemID, Status: protocol.BatchItemRunning}); stuck.Matched != 0 {
		t.Fatalf("不该有项卡在 running，实际 %d 项", stuck.Matched)
	}
}

// TestPreviewIsReadable：摘要是一行 JSON，不是 msgpack 二进制。
func TestPreviewIsReadable(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok",
		Items: []protocol.BatchItem{{Input: mustRaw(map[string]any{"n": 21})}},
	})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 8*time.Second)
	page := mustCall[protocol.BatchResultResult](f, t, "result", protocol.BatchResultPayload{ItemID: res.ItemID})
	if len(page.Items) != 1 {
		t.Fatalf("应有 1 项，实际 %d", len(page.Items))
	}
	p := page.Items[0].Preview
	if !json.Valid([]byte(p)) {
		t.Fatalf("摘要应是合法 JSON，实际 %q", p)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(p), &got); err != nil {
		t.Fatal(err)
	}
	if got["doubled"] != float64(42) || got["n"] != float64(21) {
		t.Fatalf("摘要内容不符: %v", got)
	}
}

// TestPreviewTruncated：摘要受 preview_bytes 限制。
func TestPreviewTruncated(t *testing.T) {
	f := startFixture(t, func(c *server.Config) { c.PreviewBytes = 8 })
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok",
		Items: []protocol.BatchItem{{Input: mustRaw(map[string]any{"n": 1})}},
	})
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 8*time.Second)
	page := mustCall[protocol.BatchResultResult](f, t, "result", protocol.BatchResultPayload{ItemID: res.ItemID})
	if n := len(page.Items[0].Preview); n > 8 {
		t.Fatalf("摘要应被截到 8 字节内，实际 %d: %q", n, page.Items[0].Preview)
	}
}

// TestKeyDonePersisted：任务结束后 status 里的 KeyDone 必须是真的。
func TestKeyDonePersisted(t *testing.T) {
	f := startFixture(t, nil)
	list := []protocol.BatchItem{
		{Input: mustRaw(map[string]any{"n": 0}), Key: true},
		{Input: mustRaw(map[string]any{"n": 1})},
		{Input: mustRaw(map[string]any{"n": 2}), Key: true},
		{Input: mustRaw(map[string]any{"n": 3})},
	}
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok", Items: list, Concurrency: 1,
		Wake: protocol.BatchWakePolicy{Mode: protocol.BatchWakeKey},
	})
	collect(t, f, 10*time.Second)
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 5*time.Second)
	if done.KeyTotal != 2 || done.KeyDone != 2 {
		t.Fatalf("关键项应为 2/2，实际 %d/%d", done.KeyDone, done.KeyTotal)
	}
	if !done.Woken {
		t.Fatal("关键项全部完成时该触发过唤醒")
	}
}

// TestAllKeyItemsWakeOnce：所有项都是 key、且它们是最后一批完成时，只推终态一条。
func TestAllKeyItemsWakeOnce(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 3)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"n": i}), Key: true}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok", Items: list, Concurrency: 3,
		Wake: protocol.BatchWakePolicy{Mode: protocol.BatchWakeKey},
	})
	pushes := collect(t, f, 10*time.Second)
	if len(pushes) != 1 {
		t.Fatalf("应只推一条（终态），实际 %d 条（任务 %s）: %+v", len(pushes), res.ItemID, pushes)
	}
	if !pushes[0].Final || pushes[0].Phase != protocol.BatchPhaseDone {
		t.Fatalf("推的应是终态 done，实际 %+v", pushes[0])
	}
}

// TestResumeDuringDrain：暂停后立刻恢复，不能把没跑的项当成完成。
//
// 老实现里旧调度循环收尾时一看 paused=false 就直接落终态，剩下的项人间蒸发：
// 任务报 done，实际只跑了 1/4。
func TestResumeDuringDrain(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 4)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 200})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	time.Sleep(300 * time.Millisecond)
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID}); ctl.Affected != 1 {
		t.Fatalf("暂停应生效，实际 %+v", ctl)
	}
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, ItemID: res.ItemID}); ctl.Affected != 1 {
		t.Fatalf("恢复应生效，实际 %+v", ctl)
	}
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 15*time.Second)
	if done.Ok != 4 {
		t.Fatalf("4 项都得跑完，实际 %+v", done)
	}
	if got := f.target.count("slow"); got != 4 {
		t.Fatalf("目标应被调 4 次，实际 %d", got)
	}
}

// TestControlIdempotent：重复操作与对已结束任务操作都不报错，只回 affected=0。
func TestControlIdempotent(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 3)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 120})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	time.Sleep(60 * time.Millisecond)

	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID}); ctl.Affected != 1 {
		t.Fatalf("第一次暂停应生效，实际 %+v", ctl)
	}
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID}); ctl.Affected != 0 {
		t.Fatalf("重复暂停应回 affected=0，实际 %+v", ctl)
	}
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, ItemID: res.ItemID}); ctl.Affected != 1 {
		t.Fatalf("恢复应生效，实际 %+v", ctl)
	}
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, ItemID: res.ItemID}); ctl.Affected != 0 {
		t.Fatalf("未暂停时恢复应回 affected=0，实际 %+v", ctl)
	}
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseDone, 10*time.Second)
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID}); ctl.Affected != 0 {
		t.Fatalf("对已结束任务操应回 affected=0，实际 %+v", ctl)
	}
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpCancel, ItemID: res.ItemID}); ctl.Affected != 0 {
		t.Fatalf("对已结束任务取消应回 affected=0，实际 %+v", ctl)
	}
	// 幂等操作不能把终态改掉。
	if s := f.status(t, res.ItemID); s.Phase != protocol.BatchPhaseDone || s.Ok != 3 {
		t.Fatalf("终态不该被事后的控制改动，实际 %+v", s)
	}
}

// TestControlScopeAll：scope=all 作用到所有非终态任务。
func TestControlScopeAll(t *testing.T) {
	f := startFixture(t, nil)
	mk := func() string {
		list := make([]protocol.BatchItem, 3)
		for i := range list {
			list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 250})}
		}
		return f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1}).ItemID
	}
	a, b := mk(), mk()
	time.Sleep(80 * time.Millisecond)

	ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, Scope: protocol.BatchScopeAll})
	if ctl.Affected != 2 {
		t.Fatalf("应有 2 个任务被暂停，实际 %+v", ctl)
	}
	for _, id := range []string{a, b} {
		if s := f.status(t, id); s.Phase != protocol.BatchPhasePaused {
			t.Fatalf("%s 应转 paused，实际 %s", id, s.Phase)
		}
	}
	ctl = f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, Scope: protocol.BatchScopeAll})
	if ctl.Affected != 2 {
		t.Fatalf("应有 2 个任务被恢复，实际 %+v", ctl)
	}
	for _, id := range []string{a, b} {
		if s := f.waitPhase(t, id, protocol.BatchPhaseDone, 15*time.Second); s.Ok != 3 {
			t.Fatalf("%s 恢复后应跑完 3 项，实际 %+v", id, s)
		}
	}
}

// TestTargetUnknown：名册里没有这个服务 → BATCH_TARGET_UNKNOWN（不是 NOT_BATCHABLE）。
func TestTargetUnknown(t *testing.T) {
	f := startFixture(t, nil)
	_, err := f.callRun(t, protocol.BatchRunPayload{Service: "ghost", Action: "ok", Items: items(1)})
	if err == nil || !contains(err.Error(), "BATCH_TARGET_UNKNOWN") {
		t.Fatalf("应报 BATCH_TARGET_UNKNOWN，实际 %v", err)
	}
	_, err = f.callRun(t, protocol.BatchRunPayload{Service: "worker", Action: "no-such-action", Items: items(1)})
	if err == nil || !contains(err.Error(), "BATCH_NOT_BATCHABLE") {
		t.Fatalf("服务在册但没这个动作，应报 BATCH_NOT_BATCHABLE，实际 %v", err)
	}
}

// TestItemTimeout：单项超时算该项失败，不拖累任务。
func TestItemTimeout(t *testing.T) {
	f := startFixture(t, nil)
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "slow",
		Items: []protocol.BatchItem{
			{Input: mustRaw(map[string]any{"ms": 2000})},
			{Input: mustRaw(map[string]any{"ms": 10})},
		},
		Concurrency: 2, TimeoutMS: 200,
	})
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseFailed, 10*time.Second)
	if done.Failed != 1 || done.Ok != 1 {
		t.Fatalf("应 1 失败 1 成功，实际 %+v", done)
	}
	page := mustCall[protocol.BatchResultResult](f, t, "result", protocol.BatchResultPayload{ItemID: res.ItemID})
	var timedOut bool
	for _, it := range page.Items {
		if it.Status == protocol.BatchItemFailed && it.Error != "" {
			timedOut = true
		}
	}
	if !timedOut {
		t.Fatalf("超时项应带错误原因，实际 %+v", page.Items)
	}
}

// TestInvalidParamsRejected：参数校验用各自的错误码，不混用 BATCH_OP。
func TestInvalidParamsRejected(t *testing.T) {
	f := startFixture(t, nil)
	cases := []struct {
		name string
		p    protocol.BatchRunPayload
		code string
	}{
		{"on_error", protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(1), OnError: "maybe"}, "BATCH_ONERROR"},
		{"concurrency-high", protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(1), Concurrency: 999}, "BATCH_CONCURRENCY"},
		{"concurrency-neg", protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(1), Concurrency: -3}, "BATCH_CONCURRENCY"},
		{"wake-mode", protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(1),
			Wake: protocol.BatchWakePolicy{Mode: "later"}}, "BATCH_WAKE"},
		{"empty", protocol.BatchRunPayload{Service: "worker", Action: "ok"}, "BATCH_ITEMS"},
		{"no-service", protocol.BatchRunPayload{Action: "ok", Items: items(1)}, "BATCH_TARGET_UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := f.callRun(t, tc.p)
			if err == nil || !contains(err.Error(), tc.code) {
				t.Fatalf("应报 %s，实际 %v", tc.code, err)
			}
		})
	}
}

// TestListCallerFilter：队列可按调用方过滤（前端只看自己的）。
func TestListCallerFilter(t *testing.T) {
	f := startFixture(t, nil)
	other := service.New(service.Config{
		Name: "other", Listen: "127.0.0.1:0", Center: f.center.URL(), Logger: log.New(testWriter{t}, "[other] ", 0),
	})
	if err := other.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Shutdown)
	waitRoster(t, other, "batch")

	call := func(s *service.Service) string {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := s.Call(ctx, "batch", "run", protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(1)}, "", "boss-1")
		if err != nil {
			t.Fatal(err)
		}
		return decodeInto[protocol.BatchRunResult](t, raw).ItemID
	}
	mine := call(f.probe)
	theirs := call(other)
	f.waitPhase(t, mine, protocol.BatchPhaseDone, 8*time.Second)
	f.waitPhase(t, theirs, protocol.BatchPhaseDone, 8*time.Second)

	probe := mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{Caller: "probe"})
	if len(probe.Jobs) != 1 || probe.Jobs[0].ItemID != mine {
		t.Fatalf("按 caller=probe 过滤应只剩自己的任务，实际 %+v", probe.Jobs)
	}
	otherList := mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{Caller: "other"})
	if len(otherList.Jobs) != 1 || otherList.Jobs[0].ItemID != theirs {
		t.Fatalf("按 caller=other 过滤应只剩对方的任务，实际 %+v", otherList.Jobs)
	}
}

// TestPauseAllCountsOnlyChanged：`scope:all` 的 affected 只数"状态真变了"的任务。
func TestPauseAllCountsOnlyChanged(t *testing.T) {
	f := startFixture(t, nil)
	mk := func(ms int, n int) string {
		list := make([]protocol.BatchItem, n)
		for i := range list {
			list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": ms})}
		}
		return f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1}).ItemID
	}
	done := mk(10, 1)
	f.waitPhase(t, done, protocol.BatchPhaseDone, 8*time.Second)
	already := mk(400, 2)
	time.Sleep(60 * time.Millisecond)
	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: already}); ctl.Affected != 1 {
		t.Fatalf("先单独暂停一个，实际 %+v", ctl)
	}
	running := mk(400, 2)
	time.Sleep(60 * time.Millisecond)

	ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, Scope: protocol.BatchScopeAll})
	if ctl.Affected != 1 {
		t.Fatalf("只有 1 个任务状态会变（另两个分别是已暂停/已结束），实际 %+v", ctl)
	}
	for _, id := range []string{already, running} {
		if s := f.status(t, id); s.Phase != protocol.BatchPhasePaused {
			t.Fatalf("%s 应处于 paused，实际 %s", id, s.Phase)
		}
	}
	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpCancel, Scope: protocol.BatchScopeAll})
}

// TestListDoesNotPush：前端轮询（list）绝不能变成模型推送。
func TestListDoesNotPush(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 3)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 150})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	for i := 0; i < 50; i++ {
		_ = mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{})
	}
	pushes := collect(t, f, 10*time.Second)
	if len(pushes) != 1 || !pushes[0].Final {
		t.Fatalf("轮询 50 次后应只有 1 条终态推送，实际 %d 条（任务 %s）: %+v", len(pushes), res.ItemID, pushes)
	}
}

// TestPauseReleasesGlobalSlots：暂停排空后归还槽位，新任务能进来。
func TestPauseReleasesGlobalSlots(t *testing.T) {
	f := startFixture(t, func(c *server.Config) { c.MaxInflight = 2 })
	list := make([]protocol.BatchItem, 2)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 300})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 2})
	time.Sleep(80 * time.Millisecond)
	if _, err := f.callRun(t, protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(1)}); err == nil ||
		!contains(err.Error(), "BATCH_BUSY") {
		t.Fatalf("槽位应已占满，实际 %v", err)
	}

	if ctl := f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID}); ctl.Affected != 1 {
		t.Fatalf("暂停应生效，实际 %+v", ctl)
	}
	// 在飞的项跑完才归还槽位，所以这里要等到它能被受理为止。
	deadline := time.Now().Add(10 * time.Second)
	accepted := false
	for time.Now().Before(deadline) {
		if _, err := f.callRun(t, protocol.BatchRunPayload{
			Service: "worker", Action: "ok", Items: items(1), Concurrency: 1,
		}); err == nil {
			accepted = true
			break
		} else if !contains(err.Error(), "BATCH_BUSY") {
			t.Fatalf("不该是别的错误: %v", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !accepted {
		t.Fatal("暂停后槽位一直没归还，新任务永远进不来")
	}
}

// TestPauseKeepsPendingCancelSkips：pause 后未开始项仍 pending；cancel 后变 skipped。
func TestPauseKeepsPendingCancelSkips(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 4)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 200})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	time.Sleep(120 * time.Millisecond)
	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID})
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if s := f.status(t, res.ItemID); s.Phase == protocol.BatchPhasePaused && s.Pending > 0 {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	paused := f.status(t, res.ItemID)
	if paused.Phase != protocol.BatchPhasePaused || paused.Pending == 0 {
		t.Fatalf("暂停后应留着未开始的项，实际 %+v", paused)
	}
	pend := mustCall[protocol.BatchResultResult](f, t, "result",
		protocol.BatchResultPayload{ItemID: res.ItemID, Status: protocol.BatchItemPending})
	if pend.Matched == 0 {
		t.Fatalf("暂停不该把未开始项作废，实际 %+v", pend)
	}

	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpCancel, ItemID: res.ItemID})
	done := f.waitPhase(t, res.ItemID, protocol.BatchPhaseCancelled, 10*time.Second)
	if done.Pending != 0 || done.Skipped == 0 {
		t.Fatalf("取消后未开始项应转跳过，实际 %+v", done)
	}
	if left := mustCall[protocol.BatchResultResult](f, t, "result",
		protocol.BatchResultPayload{ItemID: res.ItemID, Status: protocol.BatchItemPending}); left.Matched != 0 {
		t.Fatalf("取消后不该还有 pending 项，实际 %d 项", left.Matched)
	}
}

// TestControlDoesNotWakeModel：用户操作只改状态，不产生任何补报。
func TestControlDoesNotWakeModel(t *testing.T) {
	f := startFixture(t, nil)
	list := make([]protocol.BatchItem, 3)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 200})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	time.Sleep(60 * time.Millisecond)
	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpPause, ItemID: res.ItemID})
	f.quiet(t, 500*time.Millisecond)
	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, ItemID: res.ItemID})
	pushes := collect(t, f, 10*time.Second)
	if len(pushes) != 1 || !pushes[0].Final {
		t.Fatalf("用户操作期间不该有中间推送，实际 %d 条: %+v", len(pushes), pushes)
	}
}

// TestUnknownJobStatus：查不存在的任务（含 TTL 清理后）→ BATCH_JOB_UNKNOWN。
func TestUnknownJobStatus(t *testing.T) {
	f := startFixture(t, nil)
	for _, action := range []string{"status", "result"} {
		if _, err := f.call(t, action, protocol.BatchStatusPayload{ItemID: "01ZZZZZZZZZZZZZZZZZZZZZZZZ"}); err == nil ||
			!contains(err.Error(), "BATCH_JOB_UNKNOWN") {
			t.Fatalf("%s 应报 BATCH_JOB_UNKNOWN，实际 %v", action, err)
		}
	}
	if _, err := f.call(t, "control", protocol.BatchControlPayload{
		Op: protocol.BatchOpPause, ItemID: "01ZZZZZZZZZZZZZZZZZZZZZZZZ"}); err == nil ||
		!contains(err.Error(), "BATCH_JOB_UNKNOWN") {
		t.Fatalf("control 应报 BATCH_JOB_UNKNOWN，实际 %v", err)
	}
}

// TestListAndResultRepeated：逐项校验 result 的状态过滤与 list 的 phase 过滤。
func TestListPhaseFilter(t *testing.T) {
	f := startFixture(t, nil)
	done := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "ok", Items: items(2)})
	f.waitPhase(t, done.ItemID, protocol.BatchPhaseDone, 8*time.Second)
	slow := make([]protocol.BatchItem, 2)
	for i := range slow {
		slow[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 400})}
	}
	live := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: slow, Concurrency: 1})

	running := mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{Phase: protocol.BatchPhaseRunning})
	if len(running.Jobs) != 1 || running.Jobs[0].ItemID != live.ItemID {
		t.Fatalf("phase=running 应只剩在跑那个，实际 %+v", running.Jobs)
	}
	if running.Jobs[0].Running == 0 && running.Jobs[0].Pending == 0 {
		t.Fatalf("在跑任务该有 running/pending 计数，实际 %+v", running.Jobs[0])
	}
	finished := mustCall[protocol.BatchListResult](f, t, "list", protocol.BatchListPayload{Phase: protocol.BatchPhaseDone})
	if len(finished.Jobs) != 1 || finished.Jobs[0].ItemID != done.ItemID {
		t.Fatalf("phase=done 应只剩已完成那个，实际 %+v", finished.Jobs)
	}
	f.waitPhase(t, live.ItemID, protocol.BatchPhaseDone, 10*time.Second)
}

// TestResumeOnStart：开着 resume_on_start 时，重启后自己接着跑（不需要人工 resume）。
func TestResumeOnStart(t *testing.T) {
	sc := startStubCenter(t)
	startTarget(t, sc)
	dbPath := t.TempDir() + "/batch.db"
	boot := func(resume bool) *server.Server {
		b := server.New(server.Config{
			Name: "batch", Listen: "127.0.0.1:0", Center: sc.URL(), DBPath: dbPath, ResumeOnStart: resume,
		}, log.New(testWriter{t}, "[batch] ", 0))
		if err := b.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return b
	}

	b1 := boot(false)
	probe := service.New(service.Config{
		Name: "probe", Listen: "127.0.0.1:0", Center: sc.URL(), Logger: log.New(testWriter{t}, "[probe] ", 0),
	})
	probe.Handle("report_result", func(context.Context, protocol.Envelope, protocol.JumpPayload) (any, error) {
		return map[string]string{"status": "accepted"}, nil
	})
	if err := probe.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe.Shutdown)
	waitRoster(t, probe, "batch")

	f := &fixture{batch: b1, center: sc, probe: probe, reports: make(chan protocol.AsyncResultPayload, 64)}
	list := make([]protocol.BatchItem, 4)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 2500})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 1})
	time.Sleep(150 * time.Millisecond)
	b1.Shutdown()
	time.Sleep(100 * time.Millisecond)

	b2 := boot(true)
	t.Cleanup(b2.Shutdown)
	probe.Shutdown()
	probe2 := service.New(service.Config{
		Name: "probe2", Listen: "127.0.0.1:0", Center: sc.URL(), Logger: log.New(testWriter{t}, "[probe2] ", 0),
	})
	if err := probe2.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(probe2.Shutdown)
	waitRoster(t, probe2, "batch")
	f.probe = probe2

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		s := f.status(t, res.ItemID)
		if s.Phase == protocol.BatchPhaseDone || s.Phase == protocol.BatchPhaseFailed {
			if s.Ok+s.Failed+s.Skipped != 4 {
				t.Fatalf("重启恢复后总数应守恒，实际 %+v", s)
			}
			if s.Ok < 3 {
				t.Fatalf("开着 resume_on_start 应自动跑完剩下的项，实际 %+v", s)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("20s 内没收敛，当前 %+v", f.status(t, res.ItemID))
}
