package server

// R-030 派发接调用账本的纯逻辑测试：phase 映射、报告指纹、report_seq 幂等，
// 以及「没有 call_id 就不补报 / call_open 失败只退化」的失败语义。

import (
	"context"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestReportPhaseOf(t *testing.T) {
	cases := []struct {
		name       string
		ownerPhase string
		final      bool
		want       string
		ok         bool
	}{
		{"受理回执", "queued", false, "accepted", true},
		{"审批通过", "approved", false, "approved", true},
		{"审批拒绝", "rejected", true, "failed", true},
		{"完成", "done", true, "done", true},
		{"失败", "failed", true, "failed", true},
		{"超时", "timeout", true, "timeout", true},
		{"取消（单 l，timer 用词）", "canceled", true, "cancelled", true},
		{"取消（双 l，S-001 用词）", "cancelled", true, "cancelled", true},
		{"大小写/空白归一", " DONE ", true, "done", true},
		// 不自洽或不在词汇表：不驱动账本，也不猜。
		{"done 却没标 final", "done", false, "", false},
		{"accepted 却标 final", "accepted", true, "", false},
		{"进度不驱动账本", "progress", false, "", false},
		{"未知词汇", "running", false, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := reportPhaseOf(tc.ownerPhase, tc.final)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("reportPhaseOf(%q, %v) = (%q, %v)，want (%q, %v)",
					tc.ownerPhase, tc.final, got, ok, tc.want, tc.ok)
			}
		})
	}
}

// TestReportFingerprintStableAndDistinct：同一份报告重试必须同指纹（复用幂等键）；
// 内容任一处变化都必须是新报告（拿新 seq，不能覆盖已入账的那条）。
func TestReportFingerprintStableAndDistinct(t *testing.T) {
	base := protocol.AsyncResultPayload{ItemID: "item-1", Phase: "done", Text: "完成", Final: true, Data: []byte{1, 2}}
	again := protocol.AsyncResultPayload{ItemID: "item-1", Phase: "done", Text: "完成", Final: true, Data: []byte{1, 2}}
	if reportFingerprint("item-1", base) != reportFingerprint("item-1", again) {
		t.Fatal("同一份报告两次的指纹必须相同")
	}
	mutations := []struct {
		name string
		make func() (string, protocol.AsyncResultPayload)
	}{
		{"换 item", func() (string, protocol.AsyncResultPayload) { return "item-2", base }},
		{"换 phase", func() (string, protocol.AsyncResultPayload) {
			p := base
			p.Phase = "failed"
			return "item-1", p
		}},
		{"换 final", func() (string, protocol.AsyncResultPayload) {
			p := base
			p.Final = false
			return "item-1", p
		}},
		{"换正文", func() (string, protocol.AsyncResultPayload) {
			p := base
			p.Text = "完成（补充）"
			return "item-1", p
		}},
		{"换 data", func() (string, protocol.AsyncResultPayload) {
			p := base
			p.Data = []byte{1, 3}
			return "item-1", p
		}},
	}
	for _, m := range mutations {
		t.Run(m.name, func(t *testing.T) {
			item, p := m.make()
			if reportFingerprint(item, p) == reportFingerprint("item-1", base) {
				t.Fatal("内容变化后指纹不应相同")
			}
		})
	}
}

// TestNextReportIdempotent：同一 call 内同一份报告复用 (id, seq)；新报告递增 seq。
func TestNextReportIdempotent(t *testing.T) {
	j := &asyncJob{}
	id1, seq1 := j.nextReport("same")
	if id1 == "" || seq1 != 1 {
		t.Fatalf("首份报告应拿到 (非空 id, seq=1)，实际 (%q,%d)", id1, seq1)
	}
	id2, seq2 := j.nextReport("same")
	if id2 != id1 || seq2 != seq1 {
		t.Fatalf("同一份报告重试必须复用 (id,seq)，实际 (%q,%d) != (%q,%d)", id2, seq2, id1, seq1)
	}
	id3, seq3 := j.nextReport("other")
	if id3 == id1 || seq3 != 2 {
		t.Fatalf("新报告应是新 id 且 seq=2，实际 (%q,%d)", id3, seq3)
	}
}

// TestRelaySkipsWithoutCallID：call_open 失败时没有 call_id——绝不补报（不用假 id）。
// 未 Start 的骨架解析不到 memory；如果这里真发了请求，会得到错误而不是 (false,nil)。
func TestRelaySkipsWithoutCallID(t *testing.T) {
	s := asyncTestServer()
	j := &asyncJob{}
	stored, err := s.relayReport(j, "k", "item-1", "done", true, "完成", nil, false)
	if stored || err != nil {
		t.Fatalf("没有 call_id 时应静默跳过（不发送、不造假），实际 stored=%v err=%v", stored, err)
	}
	if len(j.reports) != 0 || j.reportSeq != 0 {
		t.Fatalf("跳过路径不应分配 report_id/seq（reports=%v seq=%d）", j.reports, j.reportSeq)
	}
}

// TestRelayFailureIsVisible：有 call_id 但 memory 不可达时必须返回错误（不冒充已入账）。
func TestRelayFailureIsVisible(t *testing.T) {
	s := asyncTestServer()
	j := &asyncJob{memCallID: "call-1", source: "worker", cid: "c", trace: "t", name: "worker__run"}
	stored, err := s.relayReport(j, "k", "item-1", "done", true, "完成", nil, false)
	if stored || err == nil {
		t.Fatalf("memory 不可达时 relay 必须失败且 stored=false，实际 stored=%v err=%v", stored, err)
	}
}

// TestOpenToolCallFailureDegrades：call_open 失败只返回空串（派发继续），不 panic、不造假。
func TestOpenToolCallFailureDegrades(t *testing.T) {
	s := asyncTestServer()
	if got := s.openToolCall(context.Background(), "worker", "run", "trace",
		protocol.Provide{LongRunning: true}); got != "" {
		t.Fatalf("memory 不可达时 call_open 应退化返回空串，实际 %q", got)
	}
}
