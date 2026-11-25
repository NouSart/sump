package tests

// owner 侧控制面（R-029）的 batch 行为：status 认识/不认识任务；pause / resume 以账本阶段
// 确认；cancel 被受理但在飞项未跑完时如实回 running + reason（不假装已停止）。

import (
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// ownerStatusOf 发一次 owner_status 并解出结果。
func ownerStatusOf(t *testing.T, f *fixture, ids ...string) protocol.OwnerStatusResult {
	t.Helper()
	return mustCall[protocol.OwnerStatusResult](f, t, "owner_status", protocol.OwnerStatusPayload{IDs: ids})
}

// ownerControlOf 发一次 owner_control：成功解出回执，失败返回原始错误（错误码可断言）。
func ownerControlOf(t *testing.T, f *fixture, id, op string) (protocol.OwnerControlResult, error) {
	t.Helper()
	raw, err := f.call(t, "owner_control", protocol.OwnerControlPayload{ID: id, Op: op})
	if err != nil {
		return protocol.OwnerControlResult{}, err
	}
	return decodeInto[protocol.OwnerControlResult](t, raw), nil
}

// waitOwnerExec 轮询到指定执行维。
func waitOwnerExec(t *testing.T, f *fixture, id, want string, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	last := ""
	for time.Now().Before(deadline) {
		last = ownerStatusOf(t, f, id).States[id]
		if last == want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s 内 %s 没到 exec=%s（当前 %q）", within, id, want, last)
}

// TestOwnerStatusPauseResumeCancel：三种控制都以账本阶段确认；cancel 在飞项未结束时不谎称
// cancelled，最终对账必须看到 cancelled。
func TestOwnerStatusPauseResumeCancel(t *testing.T) {
	f := startFixture(t, nil)
	slowItems := []protocol.BatchItem{
		{Input: mustRaw(map[string]any{"ms": 400})},
		{Input: mustRaw(map[string]any{"ms": 400})},
		{Input: mustRaw(map[string]any{"ms": 400})},
	}
	res := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "slow", Items: slowItems, Concurrency: 1,
	})

	waitOwnerExec(t, f, res.ItemID, protocol.OwnerExecRunning, 10*time.Second)
	st := ownerStatusOf(t, f, res.ItemID, "never-seen")
	if !st.RecoveryComplete {
		t.Fatalf("Start 已完成恢复，recovery_complete 应为 true: %+v", st)
	}
	if st.States[res.ItemID] != protocol.OwnerExecRunning {
		t.Fatalf("在跑的任务执行维应为 running: %+v", st.States)
	}
	if _, ok := st.States["never-seen"]; ok {
		t.Fatalf("不认识的任务不该出现在 states 里: %+v", st.States)
	}

	ctl, err := ownerControlOf(t, f, res.ItemID, protocol.OwnerOpPause)
	if err != nil {
		t.Fatalf("pause 失败: %v", err)
	}
	if ctl.Exec != protocol.OwnerExecPaused || ctl.Reason != "" {
		t.Fatalf("pause 应确认 paused: %+v", ctl)
	}
	if phase := f.status(t, res.ItemID).Phase; phase != protocol.BatchPhasePaused {
		t.Fatalf("pause 后账本阶段应为 paused，实际 %s", phase)
	}

	ctl, err = ownerControlOf(t, f, res.ItemID, protocol.OwnerOpResume)
	if err != nil {
		t.Fatalf("resume 失败: %v", err)
	}
	if ctl.Exec != protocol.OwnerExecRunning || ctl.Reason != "" {
		t.Fatalf("resume 应确认 running: %+v", ctl)
	}

	ctl, err = ownerControlOf(t, f, res.ItemID, protocol.OwnerOpCancel)
	if err != nil {
		t.Fatalf("cancel 失败: %v", err)
	}
	switch ctl.Exec {
	case protocol.OwnerExecCancelled:
		// 已经没在飞项，取消立刻落终态——确认。
	case protocol.OwnerExecRunning:
		// 在飞项还在跑：不许说 cancelled，必须给出原因，并最终对账到 cancelled。
		if ctl.Reason == "" {
			t.Fatalf("cancel 被受理但在飞项未结束时必须说明原因: %+v", ctl)
		}
	default:
		t.Fatalf("cancel 回执应是 cancelled 或 running（真实阶段），实际 %+v", ctl)
	}
	f.waitPhase(t, res.ItemID, protocol.BatchPhaseCancelled, 15*time.Second)
	if got := ownerStatusOf(t, f, res.ItemID).States[res.ItemID]; got != protocol.OwnerExecCancelled {
		t.Fatalf("终态后执行维应为 cancelled，实际 %q", got)
	}
}

// TestOwnerControlUnknownAndTerminal：不认识的 id 回 not_found；已结束的任务回真实阶段 +
// 原因（不假装"已暂停 / 已取消"）。
func TestOwnerControlUnknownAndTerminal(t *testing.T) {
	f := startFixture(t, nil)

	for _, op := range []string{protocol.OwnerOpPause, protocol.OwnerOpResume, protocol.OwnerOpCancel} {
		if _, err := ownerControlOf(t, f, "never-seen", op); !spv2.IsCode(err, spv2.CodeNotFound) {
			t.Fatalf("不认识的 id 应回 not_found（op=%s），实际 %v", op, err)
		}
	}

	done := f.mustRun(t, protocol.BatchRunPayload{
		Service: "worker", Action: "ok",
		Items: []protocol.BatchItem{{Input: mustRaw(map[string]any{"n": 1})}},
	})
	f.waitPhase(t, done.ItemID, protocol.BatchPhaseDone, 10*time.Second)

	for _, op := range []string{protocol.OwnerOpCancel, protocol.OwnerOpPause} {
		ctl, err := ownerControlOf(t, f, done.ItemID, op)
		if err != nil {
			t.Fatalf("对已结束任务的 %s 不该报错（要回真实阶段）: %v", op, err)
		}
		if ctl.Exec != protocol.OwnerExecDone {
			t.Fatalf("已结束任务的执行维应为 done（op=%s）: %+v", op, ctl)
		}
		if ctl.Reason == "" {
			t.Fatalf("未生效的 %s 必须给出原因: %+v", op, ctl)
		}
	}
}
