// 经验网的接缝：把 memnet 包接进记忆服务的动作表与 recall。
//
// 边界：这里只做"取向量 / 拼文本"的事——网的形状、阈值、遗忘全在 memnet 包里，
// 那一块不 import 本仓库任何包，将来能整块搬走（独立仓库 / 独立服务）。
package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QAQ-awa-QAQ/sump/memory/memnet"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// ensureNetCentroids 给五个大类补定义向量（懒补齐，只做一次）。
//
// 种子向量**锚在种子句上**，永不随成员重算——所以补一次就够了；
// 失败（embedding 还没上线）静默返回，下次调用再试。
func (s *Server) ensureNetCentroids(ctx context.Context, env protocol.Envelope) {
	if s.cfg.Net == nil {
		return
	}
	s.netSeedMu.Lock()
	defer s.netSeedMu.Unlock()
	if s.netSeeded {
		return
	}
	if now := time.Now(); now.Before(s.netSeedNext) {
		return // 刚失败过，冷却中：别让每条经验都替一个挂掉的上游等满超时
	}
	pending, err := s.cfg.Net.PendingCentroids()
	if err != nil || len(pending) == 0 {
		if len(pending) == 0 {
			s.netSeeded = true
		}
		return
	}
	texts := make([]string, 0, len(pending))
	for _, n := range pending {
		texts = append(texts, n.Anchor)
	}
	vecs, model, err := s.embedTexts(ctx, env, texts, false)
	if err != nil {
		// 只在**慢失败**（超时）时冷却：那种失败每次都要等满一个 embedTimeout，
		// 而且期间所有 net 动作都堵在这把锁上。
		// 快速失败（服务没上线 / 明确报错，几毫秒）重试是免费的，不该被冷却挡住——
		// 否则 embedding 一恢复，这边还要瞎等半分钟。
		if errors.Is(err, context.DeadlineExceeded) {
			s.netSeedNext = time.Now().Add(netSeedCooldown)
		}
		return
	}
	for i, n := range pending {
		if err := s.cfg.Net.SetNodeCentroid(n.ID, vecs[i], model); err != nil {
			s.netSeedNext = time.Now().Add(netSeedCooldown)
			return
		}
	}
	s.netSeeded = true
}

// experienceLines 把"经验线头"变成给模型看的几行。
//
// 一行一条：`[N 条 · 0.42] 代表句（id=…，来自 task-…）`——成员 id 只放这里，
// 模型想看同一簇的其它条目/当时的过程，就自己去 dig。
// 网不可用（没有向量 / 没有经验）时静默返回 nil：召回不因为网而失败。
func (s *Server) experienceLines(ctx context.Context, env protocol.Envelope, query string) []string {
	if s.cfg.Net == nil || strings.TrimSpace(query) == "" {
		return nil
	}
	s.ensureNetCentroids(ctx, env)
	vecs, _, err := s.embedTexts(ctx, env, []string{query}, true)
	if err != nil || len(vecs) == 0 {
		return nil
	}
	heads, err := s.cfg.Net.LineHeads(vecs[0], nowFunc(), 4)
	if err != nil {
		s.logger.Printf("经验线头不可用（忽略本节）: %v", err)
		return nil
	}
	out := make([]string, 0, len(heads))
	for _, h := range heads {
		if h.Score <= 0 {
			continue
		}
		line := fmt.Sprintf("[%d 条 · %.2f] %s（id=%s", h.Count, h.Score, h.Text, h.Members[0])
		if h.TaskID != "" {
			line += "，来自 " + h.TaskID
		}
		line += "）"
		if len(h.Members) > 1 {
			line += fmt.Sprintf("；同簇还有 %d 条", len(h.Members)-1)
		}
		out = append(out, line)
	}
	return out
}

// actionNote 写一条经验（整理时机由调用方决定：收尾后、定时、手动都行）。
// 不进 provides：不给 LLM 随手记东西的按钮——记错的"经验"比没记更糟。
func (s *Server) actionNote(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.NotePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if s.cfg.Net == nil {
		return nil, errors.New("经验网未启用")
	}
	s.ensureNetCentroids(ctx, env)
	content := strings.TrimSpace(p.Content)
	if content == "" {
		return nil, errors.New("note: content 必填")
	}
	vecs, model, err := s.embedTexts(ctx, env, []string{content}, false)
	if err != nil {
		return nil, fmt.Errorf("note: 向量化失败（不写半条经验）: %w", err)
	}
	res, err := s.cfg.Net.Attach(nowFunc(), memnet.AttachInput{
		Content:        content,
		ConversationID: p.ConversationID,
		TaskID:         p.TaskID,
		Source:         env.From,
		Durability:     p.Durability,
	}, vecs[0], model)
	if err != nil {
		return nil, fmt.Errorf("note: %w", err)
	}
	return protocol.NoteResult{
		ID:        res.EntryID,
		NodeIDs:   res.NodeIDs,
		Duplicate: res.Duplicate,
		Skipped:   res.Skipped,
	}, nil
}

// actionDig 顺着线头往下扒：
//   - 给 entry_id：翻出这条经验同一大类里够近的兄弟（"还有没有类似的"）；
//   - 给 node_id：展开整个大类（"这一类都有什么"）。
func (s *Server) actionDig(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.DigPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if s.cfg.Net == nil {
		return nil, errors.New("经验网未启用")
	}
	now := nowFunc()
	switch {
	case p.EntryID != "":
		center, notes, total, err := s.cfg.Net.DigEntry(p.EntryID, now, p.Limit)
		if err != nil {
			return nil, fmt.Errorf("dig: %w", err)
		}
		return protocol.DigResult{
			EntryID: center.ID,
			Content: center.Content,
			TaskID:  center.TaskID,
			Notes:   noteViews(notes),
			Total:   total,
		}, nil
	case p.NodeID != "":
		node, notes, total, err := s.cfg.Net.DigNode(p.NodeID, now, p.Limit)
		if err != nil {
			return nil, fmt.Errorf("dig: %w", err)
		}
		return protocol.DigResult{
			NodeID:  node.ID,
			Content: node.Anchor,
			Notes:   noteViews(notes),
			Total:   total,
		}, nil
	default:
		return nil, errors.New("dig: 需要 entry_id 或 node_id")
	}
}

// actionLineHeads 直接要线头（门户/调试用；recall 里是自动带的）。
func (s *Server) actionLineHeads(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.LineHeadsPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if s.cfg.Net == nil {
		return nil, errors.New("经验网未启用")
	}
	s.ensureNetCentroids(ctx, env)
	vecs, _, err := s.embedTexts(ctx, env, []string{p.Query}, true)
	if err != nil {
		return nil, fmt.Errorf("lineheads: 向量化失败: %w", err)
	}
	heads, err := s.cfg.Net.LineHeads(vecs[0], nowFunc(), p.Limit)
	if err != nil {
		return nil, fmt.Errorf("lineheads: %w", err)
	}
	out := make([]protocol.LineHeadView, 0, len(heads))
	for _, h := range heads {
		out = append(out, protocol.LineHeadView{
			NodeID:  h.NodeID,
			Text:    h.Text,
			Count:   h.Count,
			Score:   h.Score,
			TaskID:  h.TaskID,
			Members: h.Members,
		})
	}
	return protocol.LineHeadsResult{Heads: out}, nil
}

// actionNetPrune 维护：软剪枝（只标状态、只断干枯的线，不删行）。
// 不绑时机——谁调都行（收尾后 / 定时 / 门户手动）。
func (s *Server) actionNetPrune(_ context.Context, _ protocol.Envelope, _ protocol.JumpPayload) (any, error) {
	if s.cfg.Net == nil {
		return nil, errors.New("经验网未启用")
	}
	res, err := s.cfg.Net.Prune(nowFunc())
	if err != nil {
		return nil, fmt.Errorf("prune: %w", err)
	}
	return protocol.PruneResult{Entries: res.Entries, Links: res.Links}, nil
}

func noteViews(notes []memnet.Note) []protocol.NoteView {
	out := make([]protocol.NoteView, 0, len(notes))
	for _, n := range notes {
		out = append(out, protocol.NoteView{
			ID:         n.ID,
			Content:    n.Content,
			TaskID:     n.TaskID,
			Source:     n.Source,
			Durability: n.Durability,
			Hits:       n.Hits,
		})
	}
	return out
}
