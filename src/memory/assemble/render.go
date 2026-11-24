package assemble

import (
	"fmt"
	"sort"
	"strings"
)

// taskPrompt 与 reasoner 的旧实现（infer.go 的 taskPrompt）同一段文字：
// 迁移到 memory 后模型看到的系统提示不变，双跑比对才不会被任务提示的字面差异干扰。
func taskPrompt(name, id string) string {
	if name == "" && id == "" {
		return "" // 没有任务就不编一个：任务提示缺失是调用方的问题，不在这里造
	}
	if name == "" {
		name = "未命名任务"
	}
	if id == "" {
		return fmt.Sprintf("当前任务：《%s》。你的所有动作都属于这个任务；换任务时上下文会重新开始。", name)
	}
	return fmt.Sprintf("当前任务：《%s》（id=%s）。你的所有动作都属于这个任务；换任务时上下文会重新开始。", name, id)
}

// memoryBlock 组装记忆块（不含【记忆】前缀，前缀由 appendSystem 加）。
// 顺序即稳定/易变分层：核心记忆（稳定）→ 相关记忆 → 经验线头 → 其他会话（易变）。
// 返回 (块文本, 段落摘要)；摘要进 provenance，例如 "core=2,relevant=1,experience=3"。
func (b *builder) memoryBlock() (string, string) {
	var sections []string
	var desc []string

	core := selectMemories(b.in.Memories, true)
	if lines := b.memorySection("核心记忆", core, b.cfg.CoreLimit, b.cfg.CoreMaxChars); len(lines) > 0 {
		sections = append(sections, "（核心记忆）\n"+strings.Join(lines, "\n"))
		desc = append(desc, fmt.Sprintf("core=%d", len(lines)))
	}

	relevant := selectMemories(b.in.Memories, false)
	if lines := b.memorySection("相关记忆", relevant, b.cfg.RelLimit, b.cfg.RelMaxChars); len(lines) > 0 {
		sections = append(sections, "（相关记忆）\n"+strings.Join(lines, "\n"))
		desc = append(desc, fmt.Sprintf("relevant=%d", len(lines)))
	}

	if len(b.in.Experience) > 0 {
		heads := sortedExperience(b.in.Experience)
		lines := make([]string, 0, len(heads))
		for _, h := range heads {
			lines = append(lines, experienceLine(h))
		}
		// 引导语与 compose.BuildMemory 同文：模型要知道“想深挖就按 id 去 dig”。
		sections = append(sections, "（经验线头）\n"+
			"以下是以前干过的同类事，每条一行；想深挖就调 memory__dig（按 id）。\n"+
			strings.Join(lines, "\n"))
		desc = append(desc, fmt.Sprintf("experience=%d", len(heads)))
	}

	if len(b.in.Shared) > 0 {
		lines := make([]string, 0, len(b.in.Shared))
		for _, s := range b.in.Shared {
			lines = append(lines, sharedLine(s))
		}
		// 同一段边界说明：别的会话只有摘要，读不到对话与工具输出。
		sections = append(sections, "（其他会话）\n"+
			"这些是别的会话**在做什么**的摘要；你看不到它们的对话、工具输出与文件。\n"+
			strings.Join(lines, "\n"))
		desc = append(desc, fmt.Sprintf("shared=%d", len(b.in.Shared)))
	}

	return strings.Join(sections, "\n\n"), strings.Join(desc, ",")
}

// memorySection 渲染一类记忆，并把字符预算导致的截断/丢弃记进 warnings。
// 条数上限（limit）是调用方配置的取舍，不算失败，不 warning；字符预算越界才 warning。
func (b *builder) memorySection(label string, entries []MemoryEntry, limit, maxChars int) []string {
	r := renderMemoryLines(entries, limit, maxChars)
	if r.truncated > 0 || r.dropped > 0 {
		b.warnf("%s超出 %d 字预算：截断 %d 条、丢弃 %d 条", label, maxChars, r.truncated, r.dropped)
	}
	return r.lines
}

// memoryLines 是记忆渲染的统计（供 warning 说明“截断几条、丢弃几条”）。
type memoryLines struct {
	lines     []string
	truncated int
	dropped   int
}

// renderMemoryLines 把记忆条目渲染成“- [kind] content”，按调用方顺序取前 limit 条，
// 再按 maxChars（渲染后字符数，不含截断标记）做预算：
//   - 放得下就整条放；
//   - 放不下就把当前这条截到剩余预算并加标记（保证预算硬上限）；
//   - 之后放不下的整条丢弃。
//
// 预算 <= 0 表示不限制。
func renderMemoryLines(entries []MemoryEntry, limit, maxChars int) memoryLines {
	if limit > 0 && len(entries) > limit {
		entries = entries[:limit]
	}
	var r memoryLines
	used := 0
	for _, m := range entries {
		line := "- [" + m.Kind + "] " + m.Content
		if maxChars <= 0 {
			r.lines = append(r.lines, line)
			continue
		}
		remaining := maxChars - used
		if remaining <= 0 {
			r.dropped++
			continue
		}
		cost := runeLen(line)
		if cost <= remaining {
			r.lines = append(r.lines, line)
			used += cost
			continue
		}
		// 截断到剩余预算，并打标记（标记本身不计入预算，避免把预算算成负数）。
		runes := []rune(line)
		k := remaining
		if k > len(runes) {
			k = len(runes)
		}
		r.lines = append(r.lines, string(runes[:k])+TruncationMark)
		used = maxChars
		r.truncated++
	}
	return r
}

// selectMemories 按 Core 标志选出一类记忆（保持调用方给的顺序）。
func selectMemories(all []MemoryEntry, core bool) []MemoryEntry {
	var out []MemoryEntry
	for _, m := range all {
		if m.Core == core {
			out = append(out, m)
		}
	}
	return out
}

// sortedExperience 按 Score 降序稳定排序（同分按 Count 降序，再同分保持调用方顺序）。
// 复制一份再排，绝不改 Input 里的切片。
func sortedExperience(heads []LineHead) []LineHead {
	out := append([]LineHead(nil), heads...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return false
	})
	return out
}

// experienceLine 渲染一条经验线头，与 server/net.go 的 experienceLines 同形：
// “[N 条 · 0.42] 代表句（来自 task-…）”。
func experienceLine(h LineHead) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "[%d 条 · %.2f] %s", h.Count, h.Score, strings.TrimSpace(h.Text))
	if h.TaskID != "" {
		sb.WriteString("（来自 " + h.TaskID + "）")
	}
	return sb.String()
}

// sharedLine 渲染一条“别的会话在做什么”的摘要，与 server/share.go 的 sessionLines 同形：
// “- 门户「标题」：目标；结果（用到 a/b）”。
func sharedLine(d SessionDigest) string {
	name := strings.TrimSpace(d.Title)
	if name == "" {
		name = "未命名会话"
	}
	var sb strings.Builder
	sb.WriteString("- ")
	sb.WriteString(entryLabel(d.Entry))
	sb.WriteString("「")
	sb.WriteString(name)
	sb.WriteString("」")
	if body := joinDigest(d.Goal, d.Outcome); body != "" {
		sb.WriteString("：")
		sb.WriteString(body)
	}
	if len(d.Capabilities) > 0 {
		sb.WriteString("（用到 ")
		sb.WriteString(strings.Join(d.Capabilities, "/"))
		sb.WriteString("）")
	}
	return sb.String()
}

// joinDigest 把“在做什么”与“做到哪了”接成一句（去重、去空），与 server/share.go 同口径。
func joinDigest(goal, outcome string) string {
	g := strings.TrimSpace(goal)
	o := strings.TrimSpace(outcome)
	switch {
	case g == "":
		return o
	case o == "" || g == o:
		return g
	case strings.Contains(g, o):
		return g
	default:
		return g + "；" + o
	}
}

// entryLabel 把入口服务名翻成人话（未知入口原样显示），与 server/share.go 同口径。
func entryLabel(entry string) string {
	switch strings.ToLower(strings.TrimSpace(entry)) {
	case "web", "frontend":
		return "门户"
	case "qq":
		return "QQ"
	case "shell":
		return "终端"
	case "":
		return "会话"
	default:
		return entry
	}
}

// truncateText 按字符（rune，不切坏 UTF-8）截断文本，超长时追加 TruncationMark。
// max <= 0 表示不截断。返回 (结果, 是否截断)。
func truncateText(text string, max int) (string, bool) {
	if max <= 0 {
		return text, false
	}
	runes := []rune(text)
	if len(runes) <= max {
		return text, false
	}
	return string(runes[:max]) + TruncationMark, true
}

// runeLen 返回字符数（预算按“模型看到的字数”算，不按字节）。
func runeLen(s string) int { return len([]rune(s)) }
