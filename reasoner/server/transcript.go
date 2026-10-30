package server

// 长任务怎么写进一次复盘请求：把整段消息链压成"能读懂的流水"。
//
// 这一套是照着 opencode 与 codex 的做法搬的（2026-10-18 版），两家在同一个坑里收敛到了同样的形状：
//
//  1. **不把原始消息链丢给总结模型**，而是先序列化成带角色前缀的流水
//     （`[用户]: …` / `[工具调用]: …` / `[工具输出]: …`）——模型读得懂，人也能读。
//  2. **工具输出是真正的体积来源，先砍它**：单条截断到 1250 字（opencode
//     `TOOL_OUTPUT_MAX_CHARS = 1_250`）。砍掉的是"这次终端的完整回显"，
//     留一个头就够判断"它成没成、错在哪"。
//  3. **最近的原文逐字保留**：opencode 现在默认留 15k token（`keep: 15_000`）；我们取 8k，
//     因为复盘只要看清"最近这一段怎么走的"，不需要能让下一个人接着干的完整上下文。
//  4. **切分点对齐到轮次**：opencode 的 `recentStart` 会把边界回退到一条 user 消息，
//     免得"工具调用留着、它的输出被丢了"。我们也这么做——半截的调用/结果对模型是噪音。
//  5. **更早的降级（不额外调模型）**：opencode 把更早的整段交给摘要模型；我们一次复盘只花
//     一次调用，所以对更早的做"骨架化"：用户留 500 字（codex 也是这个取舍：用户说的话
//     不能丢），助手留 300 字，工具调用只留名字与参数开头，工具输出留 200 字——
//     **不能全丢**：坑的证据（报错原文）就藏在工具输出里，全丢了复盘就只能写空话。
//  6. **硬上限**：整段流水超过 40k token 就从最老的开始丢，并明确写一行
//     `[更早的 N 条记录已省略]`——让模型知道前面还有，不要假装看到了全部。
//
// token 数是估的（CJK 按 1 字 1 token，其余按 4 字符 1 token）。opencode 会用
// provider 回报的 usage 当锚点（`estimatePrompt` 的 measured + estimated），
// 我们没有那条线，估错的代价只是"留得比预期多/少一点"，不值得为它引一个 tokenizer。

import (
	"fmt"
	"strings"

	"github.com/NouSart/sump/reasoner/llm"
)

const (
	// reflectToolOutputChars 是单条工具输出留在流水里的上限（对齐 opencode 的 1250）。
	reflectToolOutputChars = 1_250
	// reflectKeepTokens 是"最近的原文逐字保留"的预算。opencode 默认 15k（要能让下一个人
	// 接着干）；我们只要够看清最近这一段，8k 足够，剩下的预算留给更早的骨架。
	reflectKeepTokens = 8_000
	// reflectTotalTokens 是整段流水的硬上限：再长就从最老的开始丢。
	reflectTotalTokens = 40_000
	// reflectOldUserChars 是骨架化后用户发言保留的字数：用户说的话是需求本身，
	// 比助手说的重要得多（codex 甚至把用户消息整条留在摘要里）。
	reflectOldUserChars = 500
	// reflectOldAssistantChars 是骨架化后助手发言保留的字数。
	reflectOldAssistantChars = 300
	// reflectOldToolArgChars 是骨架化后工具参数保留的字数。
	reflectOldToolArgChars = 100
	// reflectOldToolOutputChars 是骨架化后工具输出保留的字数。**不能是 0**：
	// 报错原文是"坑"的唯一证据，丢干净了复盘只会写出"要注意兼容性"这种空话。
	reflectOldToolOutputChars = 200
)

// approxTokens 估算文本的 token 数：CJK 按 1 字 1 token，其余按 4 字符 1 token。
func approxTokens(s string) int {
	var cjk, other int
	for _, r := range s {
		switch {
		case r >= 0x2E80 && r <= 0x9FFF, // CJK 部首扩展 / 统一表意文字
			r >= 0xF900 && r <= 0xFAFF, // 兼容表意文字
			r >= 0xFF00 && r <= 0xFFEF, // 全角标点与字母
			r >= 0x3000 && r <= 0x303F: // CJK 标点
			cjk++
		default:
			other++
		}
	}
	return cjk + (other+3)/4
}

// clip 按**字符**截断（不是字节）：中文被拦腰砍成半个字会变成乱码。
func clip(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "…"
}

// toolCallLine 把一次工具调用序列化：`名字(参数)`，参数是 JSON。
func toolCallLine(call llm.ToolCall, argBudget int) string {
	args := call.Function.Arguments
	if argBudget > 0 {
		args = clip(args, argBudget)
	}
	return fmt.Sprintf("[工具调用]: %s(%s)", call.Function.Name, args)
}

// transcriptItem 是流水里的一行（含它自己的 token 估算，供预算裁剪用）。
type transcriptItem struct {
	text   string
	tokens int
	turn   int  // 属于第几轮（一条用户消息开启一轮）
	cut    bool // 是否已被骨架化（"更早的部分"降级过）
}

// buildTranscript 把一段消息链压成复盘用的流水。
//
// finalAnswer 是这次交付的最终答复（它不在 messages 里——成功路径下模型最后那句
// 只被交付出去，没有回写黑板）。
func buildTranscript(messages []llm.Message, finalAnswer string) (string, int) {
	return renderItems(buildItems(messages, finalAnswer))
}

// buildItems 组装流水并做"逐字/骨架"分段（不含总预算裁剪）。
func buildItems(messages []llm.Message, finalAnswer string) []transcriptItem {
	items := make([]transcriptItem, 0, len(messages)+1)
	turn := 0
	push := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		items = append(items, transcriptItem{text: s, tokens: approxTokens(s), turn: turn})
	}

	for _, m := range messages {
		switch m.Role {
		case "system":
			// 系统提示不进流水：它是我们的脚手架，不是这次干了什么。
			continue
		case "user":
			// 工具结果在黑板里也是 role=user（外部世界的回报），要分开渲染——
			// 它属于本轮，不算新的一轮。
			if strings.HasPrefix(m.Content, "[工具结果]") {
				push("[工具输出]: " + clip(m.Content, reflectToolOutputChars))
				continue
			}
			turn++
			push("[用户]: " + m.Content)
		case "assistant":
			if strings.TrimSpace(m.Content) != "" {
				push("[助手]: " + m.Content)
			}
			for _, call := range m.ToolCalls {
				push(toolCallLine(call, reflectOldToolArgChars*4))
			}
		case "tool":
			push("[工具输出]: " + clip(m.Content, reflectToolOutputChars))
		}
	}
	if strings.TrimSpace(finalAnswer) != "" {
		push("[最终答复]: " + finalAnswer)
	}

	// 从后往前数到 keepTokens：这段里的原样保留，更早的骨架化。
	keepFrom := len(items)
	used := 0
	for i := len(items) - 1; i >= 0; i-- {
		if used+items[i].tokens > reflectKeepTokens {
			break
		}
		used += items[i].tokens
		keepFrom = i
	}
	// 边界回退到轮首：不然会出现"工具调用留着、它的输出被骨架化"这种半截状态。
	for keepFrom > 0 && items[keepFrom-1].turn == items[keepFrom].turn && items[keepFrom].turn > 0 {
		keepFrom--
	}
	for i := 0; i < keepFrom; i++ {
		items[i].text = skeleton(items[i].text)
		items[i].tokens = approxTokens(items[i].text)
		items[i].cut = true
	}
	return items
}

// renderItems 套总预算并把流水拼成文本：超了就从最老的开始丢。
func renderItems(items []transcriptItem) (string, int) {
	total := 0
	for _, it := range items {
		total += it.tokens
	}
	dropped := 0
	for total > reflectTotalTokens && len(items)-dropped > 1 {
		total -= items[dropped].tokens
		dropped++
	}
	var b strings.Builder
	if dropped > 0 {
		fmt.Fprintf(&b, "[更早的 %d 条记录已省略]\n", dropped)
	}
	for _, it := range items[dropped:] {
		b.WriteString(it.text)
		b.WriteString("\n")
	}
	return b.String(), approxTokens(b.String())
}

// skeleton 把一行流水降级成"骨架"：留角色、留开头，丢掉细节。
func skeleton(line string) string {
	prefix, body, ok := strings.Cut(line, ": ")
	if !ok {
		return clip(line, reflectOldAssistantChars)
	}
	switch {
	case strings.HasPrefix(prefix, "[工具输出]"):
		// 不能整条丢掉：报错原文是"坑"的唯一证据。
		return prefix + ": " + clip(body, reflectOldToolOutputChars) + "（更早的输出已截断）"
	case strings.HasPrefix(prefix, "[工具调用]"):
		return clip(line, reflectOldToolArgChars+20)
	case strings.HasPrefix(prefix, "[用户]"):
		return prefix + ": " + clip(body, reflectOldUserChars)
	default:
		return prefix + ": " + clip(body, reflectOldAssistantChars)
	}
}
