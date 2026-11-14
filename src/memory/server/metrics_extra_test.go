package server

// 指标不是许愿：cancelled 与 update 的推进都要真的记；同包单测守住低基数标签。

import (
	"testing"

	"github.com/NouSart/sump/spv2"
)

func TestRecordWorkTransitionMetric(t *testing.T) {
	reg := spv2.NewRegistry()
	recordWorkTransitionTo(reg, "done")
	recordWorkTransitionTo(reg, "cancelled")
	seen := map[string]bool{}
	for _, m := range reg.Snapshot() {
		if m.Name == "sump_work_transitions_total" {
			seen[m.Labels["status"]] = true
		}
	}
	if !seen["done"] || !seen["cancelled"] {
		t.Fatalf("work 状态推进指标缺 to=done/cancelled: %+v", reg.Snapshot())
	}
}
