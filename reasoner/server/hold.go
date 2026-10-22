package server

// 用户停手（user_hold）：入口服务把"用户在界面上暂停/停止了任务"这件事立刻告诉推理侧。
//
// 三档语义（见 batch/DESIGN.md §3.10）：
//   - primary  ：全部操作且碰到非常驻任务 → 停手，只等用户下一条消息；
//   - secondary：单个/部分操作且碰到非常驻任务 → 停手，用户不说话时等其他非常驻任务
//     全部结束后兜底唤醒；
//   - none     ：只碰到常驻任务 → 不停手，附注随下一次汇报带出去。
//
// 边界：这个动作**不进 provides、不是模型工具**——它是入口服务用的控制面。

import (
	"context"
	"errors"
	"strings"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// maxPendingNotes 是随消息一起送出的附注上限（超出保留最新的）。
const maxPendingNotes = 20

// holdState 是一个会话的停手状态。
type holdState struct {
	level  string          // none | secondary | primary
	paused map[string]bool // 被用户暂停/停止的异步句柄（不再算"可执行"）
	notes  []string        // 攒着等下一次投递的附注
}

// holdFor 返回某会话的停手状态（懒创建）。
func (s *Server) holdFor(cid string) *holdState {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	h, ok := s.holds[cid]
	if !ok {
		h = &holdState{level: protocol.UserHoldNone, paused: map[string]bool{}}
		s.holds[cid] = h
	}
	return h
}

// actionUserHold 承接入口服务的"用户停手"。
func (s *Server) actionUserHold(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.UserHoldPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	cid := strings.TrimSpace(p.ConversationID)
	if cid == "" {
		return nil, errors.New("user_hold: conversation_id 必填")
	}
	level := strings.ToLower(strings.TrimSpace(p.Hold))
	switch level {
	case "", protocol.UserHoldNone:
		level = protocol.UserHoldNone
	case protocol.UserHoldSecondary, protocol.UserHoldPrimary:
	default:
		return nil, errors.New("user_hold: hold 只能是 none / secondary / primary")
	}

	s.holdMu.Lock()
	h := s.holds[cid]
	if h == nil {
		h = &holdState{paused: map[string]bool{}}
		s.holds[cid] = h
	}
	// 一级优先：已经 primary 时不被 secondary 降级。
	if level == protocol.UserHoldPrimary || h.level != protocol.UserHoldPrimary {
		h.level = level
	}
	for _, id := range p.PausedItems {
		if id = strings.TrimSpace(id); id != "" {
			h.paused[id] = true
		}
	}
	h.notes = append(h.notes, p.Notes...)
	if len(h.notes) > maxPendingNotes {
		h.notes = h.notes[len(h.notes)-maxPendingNotes:]
	}
	pending := len(h.notes)
	cur := h.level
	s.holdMu.Unlock()

	s.logger.Printf("会话 %s：用户停手 level=%s，暂停句柄 %d 个，待投递附注 %d 条", cid, cur, len(p.PausedItems), pending)

	if cur == protocol.UserHoldSecondary && !s.hasExecutableAsync(cid) {
		// 没有别的可执行任务了：兜底条件当场成立，立刻唤醒（把附注送出去）。
		s.releaseHold(cid, "没有其他可执行任务")
	}
	return map[string]any{"status": "accepted", "level": cur}, nil
}

// hasExecutableAsync 判断某会话还有没有"可执行"的异步任务（非暂停、非常驻）。
func (s *Server) hasExecutableAsync(cid string) bool {
	h := s.holdFor(cid)
	s.holdMu.Lock()
	paused := make(map[string]bool, len(h.paused))
	for k, v := range h.paused {
		paused[k] = v
	}
	s.holdMu.Unlock()

	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	for item, j := range s.asyncJobs {
		if j.cid != cid || paused[item] {
			continue
		}
		if j.kind == protocol.BatchKindResident {
			continue // 常驻任务不算可执行：等它等于永远等不到
		}
		return true
	}
	return false
}

// heldFor 判断某会话当前是否处于停手状态（停手期间不派发新工具、不开新轮次）。
func (s *Server) heldFor(cid string) bool {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	h := s.holds[cid]
	return h != nil && h.level != protocol.UserHoldNone && h.level != ""
}

// takeNotes 取走某会话攒着的附注（取走即清空）。
func (s *Server) takeNotes(cid string) []string {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	h := s.holds[cid]
	if h == nil || len(h.notes) == 0 {
		return nil
	}
	out := h.notes
	h.notes = nil
	return out
}

// releaseHold 解除停手并把附注作为一次唤醒送出去（次级兜底用）。
func (s *Server) releaseHold(cid, why string) {
	notes := s.takeNotes(cid)
	s.holdMu.Lock()
	if h := s.holds[cid]; h != nil {
		h.level = protocol.UserHoldNone
	}
	s.holdMu.Unlock()
	if len(notes) == 0 {
		return
	}
	s.wakeWithNotes(cid, why, notes)
}

// clearHold 用户下一条消息到达时解除停手（附注由消息本身带走）。
func (s *Server) clearHold(cid string) {
	s.holdMu.Lock()
	if h := s.holds[cid]; h != nil {
		h.level = protocol.UserHoldNone
		h.paused = map[string]bool{}
	}
	s.holdMu.Unlock()
}

// noteAsyncDone 每个异步任务终结时调用：次级停手的兜底在这里触发。
func (s *Server) noteAsyncDone(cid string) {
	if !s.secondaryHeld(cid) {
		return
	}
	if s.hasExecutableAsync(cid) {
		return
	}
	s.releaseHold(cid, "其他非常驻任务已全部结束")
}

// secondaryHeld 判断是否处于次级停手。
func (s *Server) secondaryHeld(cid string) bool {
	s.holdMu.Lock()
	defer s.holdMu.Unlock()
	h := s.holds[cid]
	return h != nil && h.level == protocol.UserHoldSecondary
}

// renderNotes 把附注拼成前缀块（首行必须写明"这不是用户现在说的话"）。
func renderNotes(notes []string) string {
	if len(notes) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("（系统提示：以下是你在上次回复之后、用户在界面上直接做的操作，不是用户现在说的话。\n")
	b.WriteString("这些操作都是用户主动发起的——不是失败，也不是系统异常，不要试图“修复”它们。）\n")
	for _, n := range notes {
		n = strings.TrimSpace(n)
		if n == "" {
			continue
		}
		b.WriteString("- ")
		b.WriteString(n)
		b.WriteString("\n")
	}
	return b.String()
}

// mergedNotes 合并入口服务直接带来的附注与推理侧攒着的附注。
func (s *Server) mergedNotes(cid string, fromMessage []string) []string {
	out := append([]string(nil), fromMessage...)
	out = append(out, s.takeNotes(cid)...)
	if len(out) > maxPendingNotes {
		out = out[len(out)-maxPendingNotes:]
	}
	return out
}

// wakeWithNotes 用一批附注唤醒某个会话（次级兜底的唤醒，不是用户消息）。
func (s *Server) wakeWithNotes(cid, why string, notes []string) {
	text := renderNotes(notes) + "\n（" + why + "。你现在可以决定下一步：继续、恢复、还是问用户。）"

	if st := s.boardGet(cid); st != nil {
		st.mu.Lock()
		if !st.closed {
			st.messages = append(st.messages, llm.Message{Role: "user", Content: text})
			st.gen++
			st.mu.Unlock()
			s.boardKick(st)
			s.logger.Printf("会话 %s：次级兜底唤醒（%s），注入 %d 条附注", cid, why, len(notes))
			return
		}
		st.mu.Unlock()
	}

	meta, ok := s.lastAsyncMeta(cid)
	if !ok {
		s.logger.Printf("会话 %s：次级兜底唤醒时找不到可用的任务元信息，附注暂存", cid)
		s.holdMu.Lock()
		if h := s.holds[cid]; h != nil {
			h.notes = append(notes, h.notes...)
		}
		s.holdMu.Unlock()
		return
	}
	st := s.boardOpen(cid, meta.boss, meta.trace, meta.taskID, meta.taskName, []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: text},
	})
	s.boardKick(st)
	s.logger.Printf("会话 %s：次级兜底唤醒（%s，新开一轮）", cid, why)
}

// lastAsyncMeta 从该会话最近的异步受理里取交付所需的信息。
func (s *Server) lastAsyncMeta(cid string) (asyncJob, bool) {
	s.asyncMu.Lock()
	defer s.asyncMu.Unlock()
	var best asyncJob
	found := false
	for _, j := range s.asyncJobs {
		if j.cid != cid {
			continue
		}
		if !found || j.at.After(best.at) {
			best, found = j, true
		}
	}
	return best, found
}
