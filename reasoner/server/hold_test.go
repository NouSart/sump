package server

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// newHoldServer 造一个只带停手表相关字段的实例：这些语义不依赖服务骨架。
func newHoldServer() *Server {
	return &Server{
		logger:    log.New(io.Discard, "", 0),
		holds:     map[string]*holdState{},
		asyncJobs: map[string]asyncJob{},
		board:     map[string]*taskState{},
	}
}

func (s *Server) testRegisterAsync(item, cid, kind string) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	s.asyncJobs[item] = asyncJob{cid: cid, kind: kind}
}

func (s *Server) testDropAsync(item string) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	delete(s.asyncJobs, item)
}

func hold(t *testing.T, s *Server, p protocol.UserHoldPayload) map[string]any {
	t.Helper()
	raw, err := protocol.EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.actionUserHold(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: raw})
	if err != nil {
		t.Fatalf("user_hold 失败: %v", err)
	}
	m, _ := out.(map[string]any)
	return m
}

func level(s *Server, cid string) string {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if h := s.holds[cid]; h != nil {
		return h.level
	}
	return ""
}

// TestHoldPrimaryNotDowngraded：一级停手不会被后续的次级操作降级。
func TestHoldPrimaryNotDowngraded(t *testing.T) {
	s := newHoldServer()
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c1", Hold: protocol.UserHoldPrimary})
	if got := level(s, "c1"); got != protocol.UserHoldPrimary {
		t.Fatalf("应为 primary，实际 %s", got)
	}
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c1", Hold: protocol.UserHoldSecondary})
	if got := level(s, "c1"); got != protocol.UserHoldPrimary {
		t.Fatalf("primary 不该被 secondary 降级，实际 %s", got)
	}
	if !s.heldFor("c1") {
		t.Fatal("停手期间 heldFor 应为真")
	}
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c1", Hold: protocol.UserHoldNone})
	if got := level(s, "c1"); got != protocol.UserHoldNone {
		t.Fatalf("应解除停手，实际 %s", got)
	}
	if s.heldFor("c1") {
		t.Fatal("解除后 heldFor 应为假")
	}
}

// TestHoldRejectsBadInput：缺会话 id、非法档位都要报错（不能静默接受）。
func TestHoldRejectsBadInput(t *testing.T) {
	s := newHoldServer()
	raw, _ := protocol.EncodePayload(protocol.UserHoldPayload{Hold: protocol.UserHoldPrimary})
	if _, err := s.actionUserHold(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: raw}); err == nil {
		t.Fatal("缺 conversation_id 应报错")
	}
	raw, _ = protocol.EncodePayload(protocol.UserHoldPayload{ConversationID: "c1", Hold: "maybe"})
	if _, err := s.actionUserHold(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: raw}); err == nil {
		t.Fatal("非法档位应报错")
	}
	if s.heldFor("c1") {
		t.Fatal("输入被拒时不该改动停手状态")
	}
}

// TestSecondaryHoldsWhileOthersRun：还有别的非常驻任务在跑 → 次级停手持续。
func TestSecondaryHoldsWhileOthersRun(t *testing.T) {
	s := newHoldServer()
	s.testRegisterAsync("a", "c1", protocol.BatchKindOnce)
	s.testRegisterAsync("b", "c1", protocol.BatchKindOnce)

	hold(t, s, protocol.UserHoldPayload{
		ConversationID: "c1", Hold: protocol.UserHoldSecondary, PausedItems: []string{"a"},
	})
	if got := level(s, "c1"); got != protocol.UserHoldSecondary {
		t.Fatalf("b 还在跑，应保持 secondary，实际 %s", got)
	}
	// 只结束一个（a 之外还剩 b）→ 不唤醒。
	s.testDropAsync("a")
	s.noteAsyncDone("c1")
	if got := level(s, "c1"); got != protocol.UserHoldSecondary {
		t.Fatalf("还有其他任务在跑时不该解除停手，实际 %s", got)
	}
	// b 也结束 → 兜底唤醒。
	s.testDropAsync("b")
	s.noteAsyncDone("c1")
	if got := level(s, "c1"); got != protocol.UserHoldNone {
		t.Fatalf("其他非常驻任务全结束应解除停手，实际 %s", got)
	}
}

// TestSecondaryFallsBackImmediately：没有别的可执行任务时，次级停手当场兜底，不干等。
func TestSecondaryFallsBackImmediately(t *testing.T) {
	s := newHoldServer()
	s.testRegisterAsync("a", "c1", protocol.BatchKindOnce)
	// 先让任务登记消失：模拟"最后那项刚好已经结束"的窗口。这时没有可用的交付上下文，
	// 附注必须原样留在队列里，不能随着一次失败的唤醒蒸发。
	s.testDropAsync("a")
	hold(t, s, protocol.UserHoldPayload{
		ConversationID: "c1", Hold: protocol.UserHoldSecondary,
		PausedItems: []string{"a"}, Notes: []string{"用户暂停了任务 a"},
	})
	if got := level(s, "c1"); got != protocol.UserHoldNone {
		t.Fatalf("没有可执行任务时应立刻兜底，实际 %s", got)
	}
	if notes := pendingNotes(s, "c1"); len(notes) != 1 {
		t.Fatalf("附注应保留待投递，实际 %v", notes)
	}
}

// TestWakeWithNotesInjectsForRunningTask：已有进行中的任务时，附注注入那一轮而不是另开。
func TestWakeWithNotesInjectsForRunningTask(t *testing.T) {
	s := newHoldServer()
	hold(t, s, protocol.UserHoldPayload{
		ConversationID: "c1", Hold: protocol.UserHoldSecondary, Notes: []string{"用户暂停了任务 a"},
	})
	// running=true 让 boardKick 提前返回：这里只验注入契约，不跑真实 LLM 循环。
	st := &taskState{cid: "c1", running: true, gen: 1, seen: 1}
	s.board["c1"] = st

	s.releaseHold("c1", "其他非常驻任务已全部结束")

	if got := level(s, "c1"); got != protocol.UserHoldNone {
		t.Fatalf("兜底唤醒后应解除停手，实际 %s", got)
	}
	st.mu.Lock()
	msgs := append([]llm.Message(nil), st.messages...)
	gen := st.gen
	st.mu.Unlock()
	if gen != 2 {
		t.Fatalf("注入后应推进一版（gen=2），实际 %d", gen)
	}
	if len(msgs) != 1 || msgs[0].Role != "user" ||
		!strings.Contains(msgs[0].Content, "用户暂停了任务 a") ||
		!strings.Contains(msgs[0].Content, "其他非常驻任务已全部结束") {
		t.Fatalf("注入内容不符: %+v", msgs)
	}
}

// TestResidentDoesNotBlockFallback：常驻任务不参与"其他任务是否全部结束"。
func TestResidentDoesNotBlockFallback(t *testing.T) {
	s := newHoldServer()
	s.testRegisterAsync("server", "c1", protocol.BatchKindResident)
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c1", Hold: protocol.UserHoldSecondary})
	if got := level(s, "c1"); got != protocol.UserHoldNone {
		t.Fatalf("只剩常驻任务时次级停手应立刻兜底，实际 %s", got)
	}
}

// TestNotesCappedKeepsNewest：附注上限按最新保留，不无界增长。
func TestNotesCappedKeepsNewest(t *testing.T) {
	s := newHoldServer()
	for i := 0; i < maxPendingNotes+5; i++ {
		hold(t, s, protocol.UserHoldPayload{
			ConversationID: "c1", Hold: protocol.UserHoldNone,
			Notes: []string{string(rune('a' + i))},
		})
	}
	notes := pendingNotes(s, "c1")
	if len(notes) != maxPendingNotes {
		t.Fatalf("应只留 %d 条，实际 %d", maxPendingNotes, len(notes))
	}
	if notes[len(notes)-1] != string(rune('a'+maxPendingNotes+4)) {
		t.Fatalf("应保留最新的，实际最后一条 %q", notes[len(notes)-1])
	}
}

// TestMergedNotesMessageFirst：消息自带的附注在前，攒着的在后。
func TestMergedNotesMessageFirst(t *testing.T) {
	s := newHoldServer()
	hold(t, s, protocol.UserHoldPayload{ConversationID: "c1", Hold: protocol.UserHoldNone, Notes: []string{"攒着的"}})
	got := s.mergedNotes("c1", []string{"消息里的"})
	if len(got) != 2 || got[0] != "消息里的" || got[1] != "攒着的" {
		t.Fatalf("合并顺序不符: %v", got)
	}
	if again := s.mergedNotes("c1", nil); len(again) != 0 {
		t.Fatalf("取走即清空，实际 %v", again)
	}
}

// TestRenderNotesSaysNotUserWords：前缀块必须写明"这不是用户现在说的话"。
func TestRenderNotesSaysNotUserWords(t *testing.T) {
	s := newHoldServer()
	hold(t, s, protocol.UserHoldPayload{
		ConversationID: "c1", Hold: protocol.UserHoldNone, Notes: []string{"用户在界面上暂停了任务 x"},
	})
	text := renderNotes(s.mergedNotes("c1", nil))
	if !strings.Contains(text, "不是用户现在说的话") {
		t.Fatalf("附注块要说清来源，实际 %q", text)
	}
	if !strings.Contains(text, "- 用户在界面上暂停了任务 x") {
		t.Fatalf("每条附注要成一行，实际 %q", text)
	}
	if renderNotes(nil) != "" {
		t.Fatal("没有附注时不该产生空块")
	}
}

// TestClearHoldResetsPaused：用户下一条消息解除停手，同时清掉暂停句柄记录。
func TestClearHoldResetsPaused(t *testing.T) {
	s := newHoldServer()
	hold(t, s, protocol.UserHoldPayload{
		ConversationID: "c1", Hold: protocol.UserHoldSecondary, PausedItems: []string{"a"},
	})
	s.clearHold("c1")
	if got := level(s, "c1"); got != protocol.UserHoldNone {
		t.Fatalf("clearHold 后应为 none，实际 %s", got)
	}
	s.holdMu.Lock()
	left := len(s.holds["c1"].paused)
	s.holdMu.Unlock()
	if left != 0 {
		t.Fatalf("暂停句柄记录应清空，实际还剩 %d 个", left)
	}
}

// pendingNotes 读某会话攒着的附注（不清空）。
func pendingNotes(s *Server, cid string) []string {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	if h := s.holds[cid]; h != nil {
		return append([]string(nil), h.notes...)
	}
	return nil
}
