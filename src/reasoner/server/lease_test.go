package server

// 同包单测：租约与 work 状态的生产接线必须在 ./server 包内也有断言，
// 否则按文件定位的异变守卫会落在没有测试的包上（存活 = 测试太弱）。

import (
	"context"
	"errors"
	"io"
	"log"
	"testing"

	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func newServerForTest() *Server {
	return New(Config{Name: "reasoner"}, log.New(io.Discard, "", 0))
}

func TestChargeBeforeToolPropagatesFailure(t *testing.T) {
	s := newServerForTest()
	var charged string
	s.leaseChargeHook = func(_ context.Context, trace string) error {
		charged = trace
		return errors.New("预算不足（测试注入）")
	}
	err := s.chargeBeforeTool(context.Background(), "w1")
	if err == nil {
		t.Fatal("扣租约失败必须返回错误（不得放行工具）")
	}
	if charged != "w1" {
		t.Fatalf("扣租约的 trace = %q, want w1", charged)
	}
	// 成功路径必须返回 nil，且仍走同一个 trace。
	s.leaseChargeHook = func(_ context.Context, trace string) error { charged = trace; return nil }
	if err := s.chargeBeforeTool(context.Background(), "w2"); err != nil {
		t.Fatalf("成功路径不应报错: %v", err)
	}
	if charged != "w2" {
		t.Fatalf("成功路径 trace = %q, want w2", charged)
	}
}

func TestBoardClosePushesWorkDone(t *testing.T) {
	s := newServerForTest()
	var got [][2]string
	s.workStatusHook = func(trace, status string) { got = append(got, [2]string{trace, status}) }
	st := &taskState{cid: "conv-1", trace: "w1"}
	s.board[st.cid] = st
	s.boardClose(st)
	if len(got) != 1 || got[0] != [2]string{"w1", "done"} {
		t.Fatalf("boardClose 应推进 work done: %v", got)
	}

	// LLM 失败 → waiting（可重试，不是终态）。
	s.workStatusHook = func(trace, status string) { got = append(got, [2]string{trace, status}) }
	s.updateWorkStatus("w2", "waiting")
	if len(got) != 2 || got[1] != [2]string{"w2", "waiting"} {
		t.Fatalf("updateWorkStatus(waiting) 未生效: %v", got)
	}
}

func TestChargeTokenLease(t *testing.T) {
	s := newServerForTest()
	var gotTokens int
	calls := 0
	s.tokenChargeHook = func(_ context.Context, trace string, tokens int) error {
		calls++
		gotTokens = tokens
		return nil
	}
	// usage 为零：不扣、不伪造。
	if err := s.chargeTokenLease(context.Background(), "w1", llm.Usage{}); err != nil {
		t.Fatalf("零 usage 不应报错: %v", err)
	}
	if calls != 0 {
		t.Fatalf("零 usage 不应扣租约，calls=%d", calls)
	}
	// usage 有值：按 total（缺省时 prompt+completion）扣。
	if err := s.chargeTokenLease(context.Background(), "w1", llm.Usage{PromptTokens: 10, CompletionTokens: 5}); err != nil {
		t.Fatalf("token 扣减失败: %v", err)
	}
	if calls != 1 || gotTokens != 15 {
		t.Fatalf("token 扣减 = calls %d tokens %d, want 1/15", calls, gotTokens)
	}
	if err := s.chargeTokenLease(context.Background(), "w1", llm.Usage{TotalTokens: 7}); err != nil {
		t.Fatalf("total 路径不应报错: %v", err)
	}
	if gotTokens != 7 {
		t.Fatalf("total 路径 tokens = %d, want 7", gotTokens)
	}
	// 扣不到：错误必须上抛（调用方会中止本轮）。
	s.tokenChargeHook = func(context.Context, string, int) error { return errors.New("预算不足") }
	if err := s.chargeTokenLease(context.Background(), "w1", llm.Usage{PromptTokens: 1}); err == nil {
		t.Fatal("token 扣减失败必须返回错误")
	}
}

func TestBoardCloseWithPendingToolIsWaiting(t *testing.T) {
	s := newServerForTest()
	var got [][2]string
	s.workStatusHook = func(trace, status string) { got = append(got, [2]string{trace, status}) }
	st := &taskState{cid: "conv-1", trace: "w1", pending: map[string]pendingJob{"call-1": {name: "slow__work"}}}
	s.board[st.cid] = st
	s.boardClose(st)
	if len(got) != 1 || got[0] != [2]string{"w1", "waiting"} {
		t.Fatalf("有在飞工具时不能标 done: %v", got)
	}
}

func TestDeliverRefIsRuntimeAgent(t *testing.T) {
	s := New(Config{Name: "reasoner"}, log.New(io.Discard, "", 0))
	from := s.deliverRef()
	if from.Kind != "agent" || from.ID != "reasoner" || from.Authority != spv2.AuthorityRuntime {
		t.Fatalf("deliverRef = %+v, want agent:reasoner/runtime", from)
	}
}
