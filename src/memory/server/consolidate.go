package server

// 记忆整理：把私有整理池里的原始观察交给 reasoner 分类，再归档到
// 共享长期记忆 / 经验网。
//
// 边界：
//   - 原文只在私有库（memory.db）与本服务的提示词里出现；
//   - 写入共享库前必须经过 shared.Sanitize，且 content 的长度有硬上限；
//   - 模型输出只当作「建议」：ID 必须来自本批候选，字段全部重新校验；
//   - 模型失败 / 输出坏掉时不写任何共享数据，候选留在池里重试（有次数上限）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/memnet"
	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
)

const (
	consolidateBatch       = 20
	consolidateExistingMax = 80
	consolidateMaxPrompt   = 14_000
	consolidateMaxContent  = 600
	consolidateTimeout     = 90 * time.Second
	consolidateMaxTokens   = 4096
	consolidateCandidateCh = 800
)

const consolidateSystem = `你是 SUMP 的记忆整理器。只输出严格的 JSON 数组，不要 markdown、不要解释。`

// consolidationDecision 是模型对一条候选的决定。字段全部来自模型，必须重新校验。
type consolidationDecision struct {
	ID         int64                   `json:"id"`
	Keep       bool                    `json:"keep"`
	Kind       string                  `json:"kind"`
	Content    string                  `json:"content"`
	Salience   float64                 `json:"salience"`
	Priority   int                     `json:"priority"`
	Durability string                  `json:"durability"`
	Reason     string                  `json:"reason"`
	Conflicts  []consolidationConflict `json:"conflicts,omitempty"`
}

// consolidationConflict 是模型对「新候选 vs 已有长期记忆」的判断。
type consolidationConflict struct {
	ID         int64  `json:"id"`
	Resolution string `json:"resolution"` // keep_new | keep_old | keep_both
	Reason     string `json:"reason"`
}

// consolidate 处理一批整理池候选；不暴露给模型（整理由收尾 / 定时 / 手动触发）。
func (s *Server) consolidate(ctx context.Context, meta callMeta) (any, error) {
	if s.staged == nil {
		return nil, errors.New("consolidate: 会话库未启用")
	}
	if s.longterm() == nil {
		return nil, errors.New("consolidate: 长期记忆未启用")
	}
	s.consolidateMu.Lock()
	defer s.consolidateMu.Unlock()
	existing, err := s.longterm().ListActive()
	if err != nil {
		return nil, fmt.Errorf("consolidate: 读取长期记忆失败: %w", err)
	}
	// 提示词只带最近的一批；冲突 id 的校验也只看这一批，避免模型引用没看见的条目。
	if len(existing) > consolidateExistingMax {
		existing = existing[len(existing)-consolidateExistingMax:]
	}
	items, err := s.staged.PendingStaged(ctx, consolidateBatch)
	if err != nil {
		return nil, fmt.Errorf("consolidate: 读取整理池失败: %w", err)
	}
	if len(items) == 0 {
		return protocol.ConsolidateResult{}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, consolidateTimeout)
	defer cancel()
	data, err := s.Call(cctx, s.cfg.Reasoner, "think", protocol.ThinkPayload{
		System:    consolidateSystem,
		Prompt:    consolidationPrompt(items, existing),
		MaxTokens: consolidateMaxTokens,
	}, meta.trace)
	if err != nil {
		s.retryStaged(ctx, items, err)
		return nil, fmt.Errorf("consolidate: reasoner.think 失败: %w", err)
	}
	var tr protocol.ThinkResult
	if err := protocol.DecodeRaw(data, &tr); err != nil {
		s.retryStaged(ctx, items, err)
		return nil, fmt.Errorf("consolidate: think 结果解析失败: %w", err)
	}

	valid := make(map[int64]bool, len(items))
	for _, it := range items {
		valid[it.ID] = true
	}
	existingByID := make(map[int64]longterm.Entry, len(existing))
	validExisting := make(map[int64]bool, len(existing))
	for _, e := range existing {
		existingByID[e.ID] = e
		validExisting[e.ID] = true
	}
	decisions, err := parseConsolidationDecisions(tr.Text, valid, validExisting)
	if err != nil {
		s.retryStaged(ctx, items, err)
		return nil, fmt.Errorf("consolidate: %w", err)
	}
	byID := make(map[int64]consolidationDecision, len(decisions))
	for _, d := range decisions {
		byID[d.ID] = d
	}

	res := protocol.ConsolidateResult{Processed: len(items)}
	for _, it := range items {
		d, ok := byID[it.ID]
		if !ok {
			if err := s.staged.MarkStaged(ctx, it.ID, "skipped", "模型未给出决定"); err != nil {
				s.logger.Printf("整理池标记失败: %v", err)
			}
			res.Skipped++
			continue
		}
		if !d.Keep {
			if err := s.staged.MarkStaged(ctx, it.ID, "skipped", "模型判定不保留"); err != nil {
				s.logger.Printf("整理池标记失败: %v", err)
			}
			res.Skipped++
			continue
		}
		if d.Durability == "transient" {
			// transient 只属于当前任务：既不进长期记忆，也不进共享经验网。
			if err := s.staged.MarkStaged(ctx, it.ID, "skipped", "仅当前任务，不归档"); err != nil {
				s.logger.Printf("整理池标记失败: %v", err)
			}
			res.Skipped++
			continue
		}
		discardNew := false
		for _, c := range d.Conflicts {
			res.Conflicts++
			if c.Resolution == "keep_old" {
				discardNew = true
			}
		}
		if discardNew {
			if err := s.staged.MarkStaged(ctx, it.ID, "skipped", "与已有记忆冲突：保留旧条目"); err != nil {
				s.logger.Printf("整理池标记失败: %v", err)
			}
			res.Skipped++
			continue
		}
		newID, _, err := s.longterm().AddWithSalience(d.Kind, d.Content, d.Priority, d.Salience, it.ConversationID, "consolidate")
		if err != nil {
			if retryErr := s.staged.RetryStaged(ctx, it.ID, err.Error()); retryErr != nil {
				s.logger.Printf("整理池重试标记失败（候选 %d）: %v", it.ID, retryErr)
			}
			res.Failed++
			continue
		}
		res.Archived++

		// 冲突只做「软删旧条目」这一种破坏性动作，且有确定性护栏：
		// 旧条目是核心、或新条目优先级/重要性不够时，旧条目保留，结果记为未解决。
		for _, c := range d.Conflicts {
			if c.Resolution != "keep_new" {
				continue
			}
			old, ok := existingByID[c.ID]
			if !ok {
				res.Unresolved++
				s.logger.Printf("冲突旧条目已不在活跃集合（候选 %d → 旧条目 %d）", it.ID, c.ID)
				continue
			}
			if old.ID == newID {
				continue
			}
			if old.Priority > 0 || d.Priority < old.Priority || d.Salience < old.Salience {
				res.Unresolved++
				s.logger.Printf("冲突未自动解决（候选 %d → 旧条目 %d）: 核心/重要性护栏", it.ID, c.ID)
				continue
			}
			removed, err := s.longterm().Forget(c.ID)
			if err != nil || !removed {
				res.Unresolved++
				s.logger.Printf("冲突旧条目软删失败（候选 %d → 旧条目 %d）: removed=%v err=%v", it.ID, c.ID, removed, err)
				continue
			}
			delete(existingByID, c.ID)
			res.Superseded++
		}

		// 经验网是可选目标：embedding 缺席/失败不能让已经归档的长期记忆回滚。
		if s.net() != nil && d.Durability != "transient" {
			if err := s.archiveConsolidated(ctx, meta, it, d); err != nil {
				res.MemnetFailed++
				s.logger.Printf("候选 %d 归档经验网失败（长期记忆已写入）: %v", it.ID, err)
			}
		}
		if err := s.staged.MarkStaged(ctx, it.ID, "done", ""); err != nil {
			s.logger.Printf("整理池标记失败（候选 %d）: %v", it.ID, err)
		}
	}
	return res, nil
}

// consolidationPrompt 把候选与已有长期记忆压成一段有界提示词。原文只在这里离开私有库，
// 不改变「共享库只收提炼物」的边界。
func consolidationPrompt(items []store.StagedItem, existing []longterm.Entry) string {
	var b strings.Builder
	b.WriteString("下面是从多个会话收集的候选原始记录。每条候选必须恰好给出一条决定：\n")
	b.WriteString(`[{"id":1,"keep":true,"kind":"偏好","content":"脱敏后的一句话","salience":0.6,"priority":0,"durability":"durable","reason":"为什么值得记","conflicts":[{"id":42,"resolution":"keep_new","reason":"旧说法已被更新"}]}]` + "\n")
	b.WriteString("规则：\n")
	b.WriteString("- 只保留跨会话仍有用的稳定事实、偏好、身份、约定或可复用经验；寒暄、临时任务细节、工具输出、模型推测一律 keep=false。\n")
	b.WriteString("- content 必须是概括句，不含路径、链接、账号、密钥、手机号、命令原文；宁可少记，不要编造。\n")
	b.WriteString("- salience 是 0..1 的重要性；priority 0=普通，1=每次对话都必须带上的核心身份/硬约束。\n")
	b.WriteString("- durability 只能是 core / durable / transient；keep=false 时其余字段可以省略。\n")
	b.WriteString("- 新候选与已有长期记忆矛盾/是同一事实的更新时，在 conflicts 里给出旧条目 id 与 resolution：keep_new（新条目取代旧条目）、keep_old（不保存新候选）、keep_both（确实需要并存）。没有冲突就省略 conflicts。\n")
	b.WriteString("- 旧条目是核心记忆，或新条目优先级/重要性不够时，系统会拒绝自动删除并保留两条；不要把 keep_new 当成无条件删除。\n")
	b.WriteString("- 不要遗漏任何候选 id，不要输出未知 id。\n\n")

	if len(existing) == 0 {
		b.WriteString("已有长期记忆：无。\n\n")
	} else {
		start := 0
		if len(existing) > consolidateExistingMax {
			start = len(existing) - consolidateExistingMax
		}
		b.WriteString("已有长期记忆（id / kind / content）：\n")
		for _, e := range existing[start:] {
			text := shared.Sanitize(strings.Join(strings.Fields(e.Content), " "), 300)
			fmt.Fprintf(&b, "[%d] %s: %s\n", e.ID, e.Kind, text)
		}
		b.WriteString("\n")
	}

	for _, it := range items {
		text := strings.Join(strings.Fields(it.Content), " ")
		if r := []rune(text); len(r) > consolidateCandidateCh {
			text = string(r[:consolidateCandidateCh]) + "…"
		}
		fmt.Fprintf(&b, "[%d] role=%s\n%s\n\n", it.ID, it.Role, text)
		if b.Len() >= consolidateMaxPrompt {
			break
		}
	}
	return b.String()
}

// parseConsolidationDecisions 解析并校验模型输出；任何未知 id / 重复 id /
// 非法字段都让整批失败——宁可重试，不带着半懂的结果写共享库。
func parseConsolidationDecisions(text string, valid, validExisting map[int64]bool) ([]consolidationDecision, error) {
	raw, err := extractJSONArray(text)
	if err != nil {
		return nil, err
	}
	var out []consolidationDecision
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("模型输出不是合法 JSON: %w", err)
	}
	if len(out) == 0 {
		return nil, errors.New("模型没有给出任何决定")
	}
	seen := make(map[int64]bool, len(out))
	for i := range out {
		d := &out[i]
		if d.ID <= 0 || !valid[d.ID] {
			return nil, fmt.Errorf("模型给出了未知候选 id=%d", d.ID)
		}
		if seen[d.ID] {
			return nil, fmt.Errorf("模型重复给出了候选 id=%d", d.ID)
		}
		seen[d.ID] = true
		if !d.Keep {
			continue
		}
		d.Kind = shared.Sanitize(strings.TrimSpace(d.Kind), 32)
		if d.Kind == "" {
			return nil, fmt.Errorf("候选 %d: keep=true 但 kind 为空", d.ID)
		}
		d.Content = shared.Sanitize(strings.TrimSpace(d.Content), consolidateMaxContent)
		if d.Content == "" {
			return nil, fmt.Errorf("候选 %d: keep=true 但 content 为空", d.ID)
		}
		if math.IsNaN(d.Salience) || math.IsInf(d.Salience, 0) || d.Salience < 0 || d.Salience > 1 {
			return nil, fmt.Errorf("候选 %d: salience 需在 0..1", d.ID)
		}
		if d.Priority < 0 || d.Priority > 3 {
			return nil, fmt.Errorf("候选 %d: priority 需在 0..3", d.ID)
		}
		if d.Durability == "" {
			d.Durability = "durable"
		}
		switch d.Durability {
		case "core", "durable", "transient":
		default:
			return nil, fmt.Errorf("候选 %d: 非法 durability %q", d.ID, d.Durability)
		}
		d.Reason = shared.Sanitize(strings.TrimSpace(d.Reason), 200)

		seenConflict := make(map[int64]bool, len(d.Conflicts))
		hasKeepOld, hasKeepNew, hasKeepBoth := false, false, false
		for j := range d.Conflicts {
			c := &d.Conflicts[j]
			if c.ID <= 0 || !validExisting[c.ID] {
				return nil, fmt.Errorf("候选 %d: 冲突指向未知长期记忆 id=%d", d.ID, c.ID)
			}
			if seenConflict[c.ID] {
				return nil, fmt.Errorf("候选 %d: 冲突条目 id=%d 重复", d.ID, c.ID)
			}
			seenConflict[c.ID] = true
			switch c.Resolution {
			case "keep_new":
				hasKeepNew = true
			case "keep_old":
				hasKeepOld = true
			case "keep_both":
				hasKeepBoth = true
			default:
				return nil, fmt.Errorf("候选 %d: 非法冲突处理 %q", d.ID, c.Resolution)
			}
			c.Reason = shared.Sanitize(strings.TrimSpace(c.Reason), 200)
		}
		if hasKeepOld && (hasKeepNew || hasKeepBoth) {
			return nil, fmt.Errorf("候选 %d: keep_old 不能与其它冲突处理并存", d.ID)
		}
		if hasKeepNew && hasKeepBoth {
			return nil, fmt.Errorf("候选 %d: keep_new 与 keep_both 不能并存", d.ID)
		}
	}
	return out, nil
}

// extractJSONArray 从可能带 markdown 围栏 / 前后解释的文本里取出数组。
func extractJSONArray(text string) ([]byte, error) {
	s := strings.TrimSpace(text)
	start := strings.Index(s, "[")
	end := strings.LastIndex(s, "]")
	if start < 0 || end <= start {
		return nil, errors.New("模型输出里没有 JSON 数组")
	}
	return []byte(s[start : end+1]), nil
}

// archiveConsolidated 把已脱敏的提炼物挂到经验网（embedding 失败只影响本节）。
func (s *Server) archiveConsolidated(ctx context.Context, meta callMeta, it store.StagedItem, d consolidationDecision) error {
	s.ensureNetCentroids(ctx, meta.trace)
	vecs, model, err := s.embedTexts(ctx, meta.trace, []string{d.Content}, false)
	if err != nil {
		return err
	}
	_, err = s.net().Attach(nowFunc(), memnet.AttachInput{
		Content:        d.Content,
		ConversationID: it.ConversationID,
		Source:         "user",
		Durability:     d.Durability,
	}, vecs[0], model)
	return err
}

// retryStaged 让失败的候选留在池里；超过上限由 store 标 failed。
func (s *Server) retryStaged(ctx context.Context, items []store.StagedItem, cause error) {
	msg := cause.Error()
	if r := []rune(msg); len(r) > 300 {
		msg = string(r[:300]) + "…"
	}
	for _, it := range items {
		if err := s.staged.RetryStaged(ctx, it.ID, msg); err != nil {
			s.logger.Printf("整理池重试标记失败（候选 %d）: %v", it.ID, err)
		}
	}
}
