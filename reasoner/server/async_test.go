package server

import (
	"context"
	"fmt"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

func asyncTestServer() *Server {
	return New(Config{Name: "reasoner", SecretFile: "-"}, log.New(io.Discard, "", 0))
}

func asyncTestTask(s *Server, cid, trace string) *taskState {
	st := s.boardOpen(cid, "", trace, "task", "test", nil)
	st.running = true // 测试只检查接收逻辑；不启动模型轮次。
	return st
}

func reserveAsync(t *testing.T, s *Server, st *taskState, callID string) *asyncJob {
	t.Helper()
	j, err := s.beginAsync("worker", st, llm.ToolCall{ID: callID, Function: llm.FunctionCall{Name: "worker__run"}})
	if err != nil {
		t.Fatal(err)
	}
	return j
}

func acceptAsyncReport(s *Server, source, trace, item, text string, final bool) (map[string]any, error) {
	raw, _ := protocol.EncodePayload(protocol.AsyncResultPayload{ItemID: item, Phase: "done", Text: text, Final: final})
	out, err := s.actionReportResult(context.Background(), protocol.Envelope{From: source, Trace: trace, To: "reasoner"}, protocol.JumpPayload{Input: raw})
	if err != nil {
		return nil, err
	}
	return out.(map[string]any), nil
}

// 同源同 trace 的两次调用，只有各自回执确定的句柄能领取缓存。
// 来源或 trace 错误不能偷走终态，也不会进入早到缓冲。
func TestAsyncEarlyReportBoundToReceipt(t *testing.T) {
	s := asyncTestServer()
	a, b := asyncTestTask(s, "a", "shared"), asyncTestTask(s, "b", "shared")
	ja, jb := reserveAsync(t, s, a, "call-a"), reserveAsync(t, s, b, "call-b")
	for _, wrong := range [][2]string{{"other", "shared"}, {"worker", "wrong"}} {
		got, err := acceptAsyncReport(s, wrong[0], wrong[1], "item-b", "wrong", true)
		if err != nil || got["status"] != "ignored" {
			t.Fatalf("unmatched report: %v %v", got, err)
		}
	}
	got, err := acceptAsyncReport(s, "worker", "shared", "item-b", "result-b", true)
	if err != nil || got["status"] != "buffered" {
		t.Fatalf("early report: %v %v", got, err)
	}
	if len(a.messages)+len(b.messages) != 0 {
		t.Fatal("receipt not received but result delivered")
	}
	if err := s.finishAsync(ja, "item-a", ""); err != nil {
		t.Fatal(err)
	}
	if len(a.messages) != 0 {
		t.Fatal("item-a consumed item-b result")
	}
	if err := s.finishAsync(jb, "item-b", ""); err != nil {
		t.Fatal(err)
	}
	if len(b.messages) != 1 || !strings.Contains(b.messages[0].Content, "result-b") || !strings.Contains(b.messages[0].Content, "job=call-b") {
		t.Fatalf("wrong buffered delivery: %+v", b.messages)
	}
	for _, final := range []bool{true, false} {
		got, err := acceptAsyncReport(s, "worker", "shared", "item-b", "late", final)
		if err != nil || got["status"] != "duplicate" {
			t.Fatalf("terminal overwritten: %v %v", got, err)
		}
	}
	if len(b.messages) != 1 {
		t.Fatal("duplicate delivered again")
	}
	if len(s.asyncJobs) != 1 || len(s.asyncEarly) != 0 || len(s.asyncPending) != 0 {
		t.Fatal("leaked reservation or result")
	}
}

func TestAsyncSourceAndTraceAfterReceipt(t *testing.T) {
	s := asyncTestServer()
	st := asyncTestTask(s, "c", "trace")
	j := reserveAsync(t, s, st, "call")
	if err := s.finishAsync(j, "item", ""); err != nil {
		t.Fatal(err)
	}
	for _, wrong := range [][2]string{{"other", "trace"}, {"worker", "wrong"}} {
		got, err := acceptAsyncReport(s, wrong[0], wrong[1], "item", "forged", true)
		if err != nil || got["status"] != "ignored" {
			t.Fatalf("wrong identity: %v %v", got, err)
		}
	}
	got, err := acceptAsyncReport(s, "worker", "trace", "item", "real", true)
	if err != nil || got["status"] != "accepted" || len(st.messages) != 1 {
		t.Fatalf("real result lost: %v %v", got, err)
	}
}

func TestAsyncRegistrationAndEarlyBufferBounded(t *testing.T) {
	s := asyncTestServer()
	st := asyncTestTask(s, "c", "trace")
	var pending []*asyncJob
	for i := 0; i < asyncRecordLimit; i++ {
		pending = append(pending, reserveAsync(t, s, st, fmt.Sprint(i)))
	}
	if _, err := s.beginAsync("worker", st, llm.ToolCall{}); err == nil {
		t.Fatal("unbounded dispatch reservations")
	}
	for i := 0; i < asyncEarlyLimit; i++ {
		if _, err := acceptAsyncReport(s, "worker", "trace", fmt.Sprint(i), "small", false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := acceptAsyncReport(s, "worker", "trace", "extra", "small", false); err == nil {
		t.Fatal("unbounded early count")
	}
	for _, j := range pending {
		s.abandonAsync(j)
	}
	if len(s.asyncPending) != 0 || len(s.asyncEarly) != 0 {
		t.Fatal("abandoned dispatch retained unmatched results")
	}
	j := reserveAsync(t, s, st, "new")
	defer s.abandonAsync(j)
	// 数量尚未满时先撞字节上限，避免大量大输出耗尽内存。
	accepted := 0
	for i := 0; i < asyncEarlyLimit; i++ {
		_, err := acceptAsyncReport(s, "worker", "trace", fmt.Sprint(i), strings.Repeat("x", 200<<10), false)
		if err != nil {
			break
		}
		accepted++
	}
	if accepted == 0 || accepted >= asyncEarlyLimit {
		t.Fatalf("byte budget not applied: %d", accepted)
	}
}

func TestAsyncEarlyPerItemAndPayloadBounded(t *testing.T) {
	s := asyncTestServer()
	st := asyncTestTask(s, "c", "trace")
	j := reserveAsync(t, s, st, "call")
	defer s.abandonAsync(j)
	for i := 0; i < asyncEarlyPerItem; i++ {
		if _, err := acceptAsyncReport(s, "worker", "trace", "item", fmt.Sprint(i), false); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := acceptAsyncReport(s, "worker", "trace", "item", "extra", false); err == nil {
		t.Fatal("unbounded per-item early messages")
	}
	if _, err := acceptAsyncReport(s, "worker", "trace", "large", strings.Repeat("x", asyncReportBytes), false); err == nil {
		t.Fatal("oversized report accepted")
	}
}

func TestAsyncDuplicateReceiptCannotReplaceOwner(t *testing.T) {
	s := asyncTestServer()
	st := asyncTestTask(s, "c", "trace")
	a, b := reserveAsync(t, s, st, "a"), reserveAsync(t, s, st, "b")
	if err := s.finishAsync(a, "same", ""); err != nil {
		t.Fatal(err)
	}
	if err := s.finishAsync(b, "same", ""); err == nil {
		t.Fatal("duplicate item_id replaced original caller")
	}
	got, err := acceptAsyncReport(s, "worker", "trace", "same", "original", true)
	if err != nil || got["status"] != "accepted" || !strings.Contains(st.messages[0].Content, "job=a") {
		t.Fatalf("ownership changed: %v %v", got, err)
	}
}

func TestAsyncOldTaskDoesNotInjectIntoReplacement(t *testing.T) {
	s := asyncTestServer()
	old := asyncTestTask(s, "c", "same-trace")
	j := reserveAsync(t, s, old, "old-call")
	if err := s.finishAsync(j, "old-item", ""); err != nil {
		t.Fatal(err)
	}
	// 即便 trace/taskID 字符串相同，新黑板实例仍不能成为旧调用的归属。
	newer := asyncTestTask(s, "c", "same-trace")
	got, err := acceptAsyncReport(s, "worker", "same-trace", "old-item", "old output", true)
	if err != nil || got["delivery"] != "dropped_no_boss" {
		t.Fatalf("wrong route: %v %v", got, err)
	}
	if s.boardGet("c") != newer || len(newer.messages) != 0 || newer.closed {
		t.Fatal("new task replaced or contaminated")
	}
}

func TestAsyncTTLAndRestartDoNotInventResults(t *testing.T) {
	s := asyncTestServer()
	st := asyncTestTask(s, "c", "trace")
	j := reserveAsync(t, s, st, "call")
	if err := s.finishAsync(j, "item", ""); err != nil {
		t.Fatal(err)
	}
	s.asyncMu.Lock()
	s.sweepAsyncLocked(j.at.Add(asyncJobTTL + time.Second))
	s.asyncMu.Unlock()
	got, err := acceptAsyncReport(s, "worker", "trace", "item", "late", true)
	if err != nil || got["status"] != "ignored" || len(st.messages) != 0 {
		t.Fatal("expired handle delivered")
	}
	fresh := asyncTestServer()
	got, err = acceptAsyncReport(fresh, "worker", "trace", "item", "late", true)
	if err != nil || got["status"] != "ignored" {
		t.Fatal("restart recovered nonexistent state")
	}
}
