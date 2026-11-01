// 重试动作的直接测试。
//
// 背景：LLM 调用失败后，原先只打日志 —— `seen` 已对齐、轮末不自跳，任务静默停住，
// 用户既没有答复也没有错误。现在失败会告诉用户、任务不关，并由门户触发 `retry` 再跑一轮。
// 这里守的是「谁能触发」与「什么情况下不该触发」。
package server

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func newRetryServer(t *testing.T) *Server {
	t.Helper()
	return &Server{
		logger:    log.New(io.Discard, "", 0),
		cfg:       Config{Name: "reasoner"},
		board:     map[string]*taskState{},
		holds:     map[string]*holdState{},
		entries:   map[string]string{},
		retryFrom: func(string) bool { return true },
	}
}

func retryPayload(t *testing.T, cid string) protocol.JumpPayload {
	t.Helper()
	raw, err := protocol.EncodePayload(protocol.RetryPayload{ConversationID: cid})
	if err != nil {
		t.Fatal(err)
	}
	return protocol.JumpPayload{Action: "retry", Input: raw}
}

// 只有门户能触发：别的服务（哪怕它在名册里）一律拒。
func TestIsPortalFrom(t *testing.T) {
	roster := []protocol.ServiceCard{{Name: "frontend"}, {Name: "qq"}, {Name: "shell"}}
	for _, tc := range []struct {
		name string
		from string
		want bool
	}{
		{name: "门户 frontend", from: "frontend", want: true},
		{name: "门户 qq", from: "qq", want: true},
		{name: "普通服务", from: "shell", want: false},
		{name: "冒充门户但不在名册", from: "frontend-2", want: false},
		{name: "空 from", from: "", want: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPortalFrom(tc.from, roster); got != tc.want {
				t.Fatalf("isPortalFrom(%q) = %v, want %v", tc.from, got, tc.want)
			}
		})
	}
	if isPortalFrom("frontend", nil) {
		t.Fatal("名册为空时不能认任何来源")
	}
}

// 非门户来源：直接拒，且错误里说明只有门户可以。
// （真实判定走 isPortalFrom + 名册，那条由 TestIsPortalFrom 覆盖；这里只测动作层对「不允许」的反应。）
func TestActionRetryRejectsNonPortal(t *testing.T) {
	s := newRetryServer(t)
	s.retryFrom = func(string) bool { return false }
	_, err := s.actionRetry(context.Background(), protocol.Envelope{From: "shell"}, retryPayload(t, "c1"))
	if err == nil || !strings.Contains(err.Error(), "只有门户") {
		t.Fatalf("非门户应被拒并说明原因，实际: %v", err)
	}
}

// 会话没有进行中的任务：说清楚没有可重试的对象。
func TestActionRetryWithoutTask(t *testing.T) {
	s := newRetryServer(t)
	_, err := s.actionRetry(context.Background(), protocol.Envelope{From: "frontend"}, retryPayload(t, "nope"))
	if err == nil || !strings.Contains(err.Error(), "没有进行中的任务") {
		t.Fatalf("缺任务应报错，实际: %v", err)
	}
}

// 任务正在跑：不许插队（否则会把 running 的那轮和水位搞乱）。
func TestActionRetryWhileRunning(t *testing.T) {
	s := newRetryServer(t)
	s.boardMu.Lock()
	s.board["c1"] = &taskState{cid: "c1", running: true, lastError: "boom"}
	s.boardMu.Unlock()
	_, err := s.actionRetry(context.Background(), protocol.Envelope{From: "frontend"}, retryPayload(t, "c1"))
	if err == nil || !strings.Contains(err.Error(), "正在跑") {
		t.Fatalf("运行中应拒绝，实际: %v", err)
	}
}

// 上一轮没失败过：没有可重试的东西。
func TestActionRetryWithoutFailure(t *testing.T) {
	s := newRetryServer(t)
	s.boardMu.Lock()
	s.board["c1"] = &taskState{cid: "c1"}
	s.boardMu.Unlock()
	_, err := s.actionRetry(context.Background(), protocol.Envelope{From: "frontend"}, retryPayload(t, "c1"))
	if err == nil || !strings.Contains(err.Error(), "没有待重试的失败") {
		t.Fatalf("无失败记录应拒绝，实际: %v", err)
	}
}

// 任务已收尾：不许把已结束的任务重开（那是新任务，不是重试）。
func TestActionRetryOnClosedTask(t *testing.T) {
	s := newRetryServer(t)
	s.boardMu.Lock()
	s.board["c1"] = &taskState{cid: "c1", closed: true, lastError: "boom"}
	s.boardMu.Unlock()
	_, err := s.actionRetry(context.Background(), protocol.Envelope{From: "frontend"}, retryPayload(t, "c1"))
	if err == nil || !strings.Contains(err.Error(), "已结束") {
		t.Fatalf("已收尾应拒绝，实际: %v", err)
	}
}

// 正常路径：清掉失败标记、推进水位，并回 {retried:true}。
func TestActionRetryAccepts(t *testing.T) {
	s := newRetryServer(t)
	// gen/seen 是并发水位：boardKick 起的轮次会立刻把 seen 追平 gen，
	// 所以只能断言「相对调用前推进了」，不能断言 gen>seen（那是竞态）。
	const genBefore = 3
	st := &taskState{cid: "c1", lastError: "boom", gen: genBefore, seen: genBefore}
	s.boardMu.Lock()
	s.board["c1"] = st
	s.boardMu.Unlock()

	out, err := s.actionRetry(context.Background(), protocol.Envelope{From: "frontend"}, retryPayload(t, "c1"))
	if err != nil {
		t.Fatalf("合法重试被拒: %v", err)
	}
	res, ok := out.(protocol.RetryResult)
	if !ok || !res.Retried || res.ConversationID != "c1" {
		t.Fatalf("回执不符: %#v", out)
	}
	st.mu.Lock()
	gen, seen, lastErr := st.gen, st.seen, st.lastError
	st.mu.Unlock()
	if lastErr != "" {
		t.Fatalf("失败标记应被清掉，实际 %q", lastErr)
	}
	if gen <= genBefore {
		t.Fatalf("水位应被推进（gen>%d），实际 gen=%d seen=%d", genBefore, gen, seen)
	}
}

// 空会话号：直接拒。
func TestActionRetryRequiresConversation(t *testing.T) {
	s := newRetryServer(t)
	_, err := s.actionRetry(context.Background(), protocol.Envelope{From: "frontend"}, retryPayload(t, "  "))
	if err == nil || !strings.Contains(err.Error(), "conversation_id 必填") {
		t.Fatalf("空会话号应被拒，实际: %v", err)
	}
}
