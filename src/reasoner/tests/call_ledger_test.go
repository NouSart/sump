package tests

// R-030：reasoner 派发接 memory 调用账本的端到端行为（真 reasoner 进程内实例 + 真 WS 对端）。
//   - 派发前 call_open：载荷带 work / service / action / mode / pausable / exec_deadline；
//   - 受理回执 relay 成 accepted 补报：Report.CallID 来自 call_open，RemoteItemID 来自 owner 句柄；
//   - owner 后续补报 relay 成 report：同一份报告重试复用 (report_id, report_seq)；
//   - call_open 失败时派发照常、补报被跳过（不用假 call_id 写账本）。

import (
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

// reportStores 返回已收到的、带 report 的 store 请求快照（可选按 phase 过滤）。
func reportStores(mem *stubMemory, phase string) []protocol.StorePayload {
	var out []protocol.StorePayload
	for _, sp := range mem.Stores() {
		if sp.Report == nil {
			continue
		}
		if phase != "" && sp.Report.Phase != phase {
			continue
		}
		out = append(out, sp)
	}
	return out
}

// waitReportStores 等到至少 n 条匹配的补报 store；超时即失败。
func waitReportStores(t *testing.T, mem *stubMemory, phase string, n int) []protocol.StorePayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := reportStores(mem, phase); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 条 phase=%q 的补报 store 超时；已有 %d 条 report store",
		n, phase, len(reportStores(mem, "")))
	return nil
}

// waitCallOpens 等到至少 n 次 call_open；超时即失败。
func waitCallOpens(t *testing.T, mem *stubMemory, n int) []protocol.CallOpenPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := mem.CallOpens(); len(got) >= n {
			return got
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待 %d 次 call_open 超时（实际 %d）", n, len(mem.CallOpens()))
	return nil
}

// ledgerScript 是这两个用例共用的剧本：派发一次异步工具 → wait → 说“已受理”。
func ledgerScript() *llm.Fake {
	return &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo ledger"}`},
		}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_wait", Type: "function",
			Function: llm.FunctionCall{Name: "wait", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "已受理，结果稍后送达"},
		{Role: "assistant", Content: "补充：完成"},
	}}
}

// TestDispatchOpensCallAndRelaysReports：一条完整的 reasoner → owner → memory 账本链。
func TestDispatchOpensCallAndRelaysReports(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "cmd-ledger")
	s, _, mem := startReportEnv(t, sc, ledgerScript())
	sendUser(t, s, "跑个账本用例")

	select {
	case <-async.got:
	case <-time.After(15 * time.Second):
		t.Fatal("异步工具未被派发")
	}
	frame := <-async.calls
	trace := frame.Work

	opens := waitCallOpens(t, mem, 1)
	op := opens[0]
	if op.Work != trace || op.Service != "stub-async" || op.Action != "run" {
		t.Fatalf("call_open 的归属/动作不符: %+v（trace=%s）", op, trace)
	}
	if op.Mode != "once" || !op.Pausable || op.ExecDeadlineMS <= 0 {
		t.Fatalf("call_open 的 mode/pausable/exec_deadline 不符: %+v", op)
	}

	// 受理回执 → accepted 补报：call_id 来自 call_open，remote_item_id 来自 owner 句柄。
	accepted := waitReportStores(t, mem, "accepted", 1)[0]
	if accepted.Report.CallID != "call-1" || accepted.Report.RemoteItemID != "cmd-ledger" ||
		accepted.Report.ReportSeq != 1 || accepted.Report.SourceService != "stub-async" ||
		accepted.Report.Final {
		t.Fatalf("accepted 补报字段不符: %+v", *accepted.Report)
	}
	if accepted.ClientItemID != "report:stub-async:"+accepted.Report.ReportID {
		t.Fatalf("补报幂等键形状不符: %q（report_id=%q）", accepted.ClientItemID, accepted.Report.ReportID)
	}
	if accepted.Boss != "stub-boss" {
		t.Fatalf("补报应带会话归属: boss=%q", accepted.Boss)
	}

	// 同一份非终态报告重试：复用同一 report_id / report_seq（memory 才能去重）。
	first := reportUntilAccepted(t, s, trace, "cmd-ledger", "approved", "审批通过，已开始执行", false)
	if first.Status != "accepted" {
		t.Fatalf("approved 补报未被受理: %+v", first)
	}
	again := fireReport(t, s, trace, "cmd-ledger", "approved", "审批通过，已开始执行", false)
	if again.Status != "accepted" {
		t.Fatalf("重试的 approved 补报应仍被受理: %+v", again)
	}
	approved := waitReportStores(t, mem, "approved", 2)
	if approved[0].Report.ReportID != approved[1].Report.ReportID ||
		approved[0].Report.ReportSeq != approved[1].Report.ReportSeq ||
		approved[0].Report.ReportSeq != 2 {
		t.Fatalf("同一份报告重试必须复用 (report_id, report_seq)=seq 2，实际 %+v / %+v",
			*approved[0].Report, *approved[1].Report)
	}

	// 终态：seq 递增、final=true，句柄仍指向 owner 的 item。
	done := reportUntilAccepted(t, s, trace, "cmd-ledger", "done", "命令执行完成", true)
	if done.Status != "accepted" {
		t.Fatalf("done 补报未被受理: %+v", done)
	}
	dones := waitReportStores(t, mem, "done", 1)
	rep := dones[0].Report
	if !rep.Final || rep.ReportSeq != 3 || rep.CallID != "call-1" || rep.RemoteItemID != "cmd-ledger" {
		t.Fatalf("done 补报字段不符: %+v", *rep)
	}
	if !strings.Contains(dones[0].Content, "命令执行完成") {
		t.Fatalf("补报正文应落库: %q", dones[0].Content)
	}

	// 终态重发：reasoner 回 duplicate，不能再写一条补报（内存记账不重复）。
	before := len(reportStores(mem, ""))
	dup := fireReport(t, s, trace, "cmd-ledger", "done", "命令执行完成", true)
	if dup.Status != "duplicate" {
		t.Fatalf("终态补报重发应回 duplicate: %+v", dup)
	}
	if after := len(reportStores(mem, "")); after != before {
		t.Fatalf("重发的终态补报不应再写账本：%d → %d", before, after)
	}
}

// TestCallOpenFailureKeepsDispatchAndSkipsLedger：call_open 失败只降级账本，
// 派发与补报交付照常；不得用假 call_id 写 report。
func TestCallOpenFailureKeepsDispatchAndSkipsLedger(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "cmd-no-ledger")
	s, _, mem := startReportEnv(t, sc, ledgerScript(), func(_ *stubBoss, m *stubMemory) {
		m.setCallOpenError("账本不可用")
	})
	sendUser(t, s, "跑个失败降级用例")

	select {
	case <-async.got:
	case <-time.After(15 * time.Second):
		t.Fatal("call_open 失败不应阻塞工具派发")
	}
	frame := <-async.calls
	trace := frame.Work
	waitCallOpens(t, mem, 1) // 确实尝试过 call_open

	// 补报仍交付给模型（受理成功），但账本里一条 report 都不能有。
	out := reportUntilAccepted(t, s, trace, "cmd-no-ledger", "done", "命令执行完成", true)
	if out.Status != "accepted" {
		t.Fatalf("没有 call_id 时补报仍应交付: %+v", out)
	}
	if got := reportStores(mem, ""); len(got) != 0 {
		t.Fatalf("没有 call_id 时不得写补报（假 id）：%+v", got[0].Report)
	}
	if got := mem.Stores(); len(got) == 0 {
		t.Fatal("补报正文仍应落进会话记录（只是不驱动账本）")
	}
}
