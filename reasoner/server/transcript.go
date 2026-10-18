package server

// 长任务怎么写进一次复盘请求：把整段消息链压成"能读懂的流水"。
//
// 这一套是照着 opencode 与 codex 的做法搬的，两家在同一个坑里收敛到了同样的形状：
//
//  1. **不把原始消息链丢给总结模型**，而是先序列化成带角色前缀的流水
//     （`[用户]: …` / `[工具调用]: …` / `[工具输出]: …`）——模型读得懂，人也能读。
//  2. **工具输出是真正的体积来源，先砍它**：单条截断到 2000 字（opencode
//     `TOOL_OUTPUT_MAX_CHARS = 2_000`，codex 同样量级）。砍掉的是"这次终端的完整
//     回显"，留一个头就够判断"它成没成、错在哪"。
//  3. **最近的原文保留，早的先降级**：opencode 保最近 8k token（`DEFAULT_KEEP_TOKENS`），
//     更早的交给摘要；我们对更早的不做二次摘要（那要多一次模型调用），而是"骨架化"：
//     用户消息全留（codex 也是这个取舍：用户说的话不能丢），助手只说前 300 字，
//     工具调用只留名字与参数开头，工具输出只留一行"已省略"。
//  4. **硬上限**：整段流水超过 40k token 就从最老的开始丢，并明确写一行
//     `[更早的 N 条记录已省略]`——让模型知道前面还有，不要假装看到了全部。
//
// token 数是估的（CJK 按 1 字 1 token，其余按 4 字符 1 token）。估错的代价是
// "留得比预期多/少一点"，不是"请求被上游拒掉"，所以不值得为它引一个 tokenizer。

import (
	"fmt"
	"strings"

	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

const (
	// reflectToolOutputChars 是单条工具输出留在流水里的上限（opencode/codex 同为 2000）。
	reflectToolOutputChars = 2_000
	// reflectKeepTokens 是"最近的原文逐字保留"的预算（对齐 opencode 的 8k）。
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
}

// buildTranscript 把一段消息链压成复盘用的流水。
//
// finalAnswer 是这次交付的最终答复（它不在 messages 里——成功路径下模型最后那句
// 只被交付出去，没有回写黑板）。
func buildTranscript(messages []llm.Message, finalAnswer string) (string, int) {
	items := make([]transcriptItem, 0, len(messages)+1)
	push := func(s string) {
		if s = strings.TrimSpace(s); s == "" {
			return
		}
		items = append(items, transcriptItem{text: s, tokens: approxTokens(s)})
	}

	for _, m := range messages {
		switch m.Role {
		case "system":
			// 系统提示不进流水：它是我们的脚手架，不是这次干了什么。
			continue
		case "user":
			// 工具结果在黑板里也是 role=user（外部世界的回报），要分开渲染。
			if strings.HasPrefix(m.Content, "[工具结果]") {
				push("[工具输出]: " + clip(m.Content, reflectToolOutputChars))
				continue
			}
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
	for i := 0; i < keepFrom; i++ {
		items[i].text = skeleton(items[i].text)
		items[i].tokens = approxTokens(items[i].text)
	}

	// 总预算：从最老的开始丢，丢掉的条数记在开头。
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
		return prefix + ": （输出已省略）"
	case strings.HasPrefix(prefix, "[工具调用]"):
		return clip(line, reflectOldToolArgChars+20)
	case strings.HasPrefix(prefix, "[用户]"):
		return prefix + ": " + clip(body, reflectOldUserChars)
	default:
		return prefix + ": " + clip(body, reflectOldAssistantChars)
	}
}
