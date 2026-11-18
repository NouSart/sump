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
