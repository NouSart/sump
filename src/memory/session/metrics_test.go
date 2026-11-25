package session

// 低基数指标的包内断言：异变守卫改 label / 砍状态时这里必须红。

import (
	"context"
	"testing"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/spv2"
)

func TestWorkTransitionMetricKeepsStatusLabel(t *testing.T) {
	reg := spv2.NewRegistry()
	recordWorkTransitionTo(reg, spv2.WorkDone)
	for _, m := range reg.Snapshot() {
		if m.Name != "sump_work_transitions_total" {
			continue
		}
		if m.Labels["status"] != spv2.WorkDone || m.Value != 1 {
			t.Fatalf("work 推进指标应带 status=%s 且计数为 1: %+v", spv2.WorkDone, m)
		}
		return
	}
	t.Fatalf("缺少 sump_work_transitions_total: %+v", reg.Snapshot())
}

func TestQueueDepthMetricCoversAllStates(t *testing.T) {
	srv, _ := startService(t)
	reg := spv2.NewRegistry()
	srv.recordQueueDepthTo(reg, context.Background())
	seen := map[string]bool{}
	for _, m := range reg.Snapshot() {
		if m.Name == "sump_queue_depth" {
			seen[m.Labels["state"]] = true
		}
	}
	for _, state := range []string{store.WorkMsgPending, store.WorkMsgClaimed, store.WorkMsgDelivered, store.WorkMsgFailed} {
		if !seen[state] {
			t.Fatalf("队列深度指标漏了 state=%s: %+v", state, reg.Snapshot())
		}
	}
}

// TestSnapshotEvictionMetric 逐出份数进 counter；nil 注册表与 0 份不产生 series。
func TestSnapshotEvictionMetric(t *testing.T) {
	reg := spv2.NewRegistry()
	recordSnapshotEvictionsTo(reg, 2)
	var got float64
	for _, m := range reg.Snapshot() {
		if m.Name == "sump_snapshot_evictions_total" {
			got = m.Value
			if len(m.Labels) != 0 {
				t.Fatalf("逐出计数不应带业务 label: %+v", m.Labels)
			}
		}
	}
	if got != 2 {
		t.Fatalf("sump_snapshot_evictions_total = %v，期望 2", got)
	}
	recordSnapshotEvictionsTo(nil, 1) // 未启动骨架：不得 panic
	recordSnapshotEvictionsTo(reg, 0) // 0 份不新增/累加
	for _, m := range reg.Snapshot() {
		if m.Name == "sump_snapshot_evictions_total" && m.Value != 2 {
			t.Fatalf("0 份不应累加: %+v", m)
		}
	}
}
