package assemble

import (
	"fmt"
	"strings"
)

// 工具配对（S-001 §3.2 / R-004）：
//   - 每个 kind=tool_call 产生一条 assistant 消息（带 tool_calls），后跟一条 role=tool
//     消息占住配对位——无论结果到没到，都不能让 tool_calls 悬空（上游会拒）；
//   - 结果按 call_id 配对，不看切片顺序（结果乱序到达是常态）；
//   - 有结果 → 结果文本（按 MaxToolOutputChars 截断并加标记）；
//   - 无结果但在 Inflight → “仍在运行”占位（不阻塞这一轮）；
//   - 无结果且不在 Inflight → “结果未知”占位 + warning（INV-4 无静默失败）；
//   - 孤立的 tool_result（找不到 call）不得进入 messages，只写 warning + provenance。

// appendTools 渲染整个工具配对块，然后清算孤立结果。
func (b *builder) appendTools(calls, results []Record) {
	inflight := inflightByCall(b.in.Inflight)
	byCall := resultsByCall(results)
	seen := make(map[string]bool, len(calls))

	for _, c := range calls {
		b.appendToolCall(c, byCall, inflight, seen)
	}
	b.orphanResults(results, seen)
}

// appendToolCall 渲染一次工具调用及其配对消息。
func (b *builder) appendToolCall(c Record, byCall map[string][]Record, inflight map[string]Inflight, seen map[string]bool) {
	callID := c.CallID
	if callID != "" && seen[callID] {
		// 同一 call_id 的重复记录是数据异常：只认第一条，第二条留证据。
		b.warnf("工具调用 %s 重复出现，已忽略后一条", callID)
		b.prov(-1, "duplicate", c.ItemID)
		return
	}
	if callID != "" {
		seen[callID] = true
	}

	name := strings.TrimSpace(c.Name)
	if name == "" {
		// 工具名缺失时退回在途记录里的 service__action（仍在跑的那条通常知道是谁）。
		if f, ok := inflight[callID]; ok && f.Service != "" {
			name = f.Service + "__" + f.Action
		}
	}
	if name == "" {
		name = "unknown"
		b.warnf("工具调用 %s 缺少工具名", toolRef(callID, c.ItemID))
	}

	// assistant.tool_calls：Content 留空——工具调用记录的 Content 是给人看的轨迹
	// （如 shell 命令 / 参数摘要），回灌给模型会被当成它自己的台词。
	idx := len(b.out.Messages)
	b.out.Messages = append(b.out.Messages, Message{
		Role:      "assistant",
		ToolCalls: []ToolCall{{ID: callID, Name: name, Arguments: c.Arguments}},
	})
	b.prov(idx, "tool_call", callID)

	// 有结果 → 用结果文本（截断 + warning）。
	if callID != "" {
		if rs := byCall[callID]; len(rs) > 0 {
			b.appendToolResult(callID, rs)
			return
		}
	}

	// 无结果但在途 → 仍在运行占位（不阻塞，也不 warning：这是正常状态）。
	if _, ok := inflight[callID]; ok {
		text := fmt.Sprintf("（工具 %s 仍在运行，结果稍后送达）", toolLabel(name, callID))
		idx = b.toolMsg(text, callID)
		b.prov(idx, "inflight", callID)
		return
	}

	// 无结果且不在途 → 结果未知占位 + warning（可能是崩溃/丢失，不能装成“还在跑”）。
	text := fmt.Sprintf("（工具 %s 结果未知：调用已发出，但未收到结果）", toolLabel(name, callID))
	idx = b.toolMsg(text, callID)
	b.warnf("工具调用 %s 结果未知（已发出但未收到结果）", toolRef(callID, c.ItemID))
	b.prov(idx, "unknown", callID)
}

// appendToolResult 用首个（Seq 最早的）结果填 tool 消息；重名结果只记 warning。
func (b *builder) appendToolResult(callID string, results []Record) {
	r := results[0]
	content := r.Content
	if b.cfg.MaxToolOutputChars > 0 {
		var truncated bool
		content, truncated = truncateText(content, b.cfg.MaxToolOutputChars)
		if truncated {
			b.warnf("工具结果 %s 超出 %d 字，已截断", toolRef(callID, r.ItemID), b.cfg.MaxToolOutputChars)
		}
	}
	if len(results) > 1 {
		b.warnf("工具调用 %s 有 %d 条结果，只用最早一条", toolRef(callID, r.ItemID), len(results))
	}
	idx := b.toolMsg(content, callID)
	b.prov(idx, "tool_result", callID)
}

// orphanResults 清算没有配对调用的结果：丢弃（不进 messages），每条 warning + provenance。
func (b *builder) orphanResults(results []Record, seen map[string]bool) {
	for _, r := range results {
		if r.CallID != "" && seen[r.CallID] {
			continue // 已被对应调用消费（重复结果在 appendToolResult 里 warning 过）
		}
		b.warnf("孤立的工具结果 %s（call_id=%s）已丢弃：找不到对应的工具调用", itemRef(r.ItemID), itemRef(r.CallID))
		b.prov(-1, "orphan", r.ItemID)
	}
}

// resultsByCall 按 call_id 分组（组内保持 Seq 升序，调用前已排好）。
func resultsByCall(results []Record) map[string][]Record {
	if len(results) == 0 {
		return nil
	}
	m := make(map[string][]Record, len(results))
	for _, r := range results {
		m[r.CallID] = append(m[r.CallID], r)
	}
	return m
}

// inflightByCall 按 call_id 建在途表；重复 key 只认第一条（输出确定）。
func inflightByCall(inflight []Inflight) map[string]Inflight {
	if len(inflight) == 0 {
		return nil
	}
	m := make(map[string]Inflight, len(inflight))
	for _, f := range inflight {
		if _, ok := m[f.CallID]; !ok {
			m[f.CallID] = f
		}
	}
	return m
}

// toolLabel 是占位文本里对“哪个工具”的称呼：优先工具名，退调用 id，再退泛指。
func toolLabel(name, callID string) string {
	if name != "" && name != "unknown" {
		return name
	}
	if callID != "" {
		return "调用 " + callID
	}
	return "该调用"
}

// toolRef 是 warning 里对“哪次调用”的称呼：call_id 与记录 id 一起给，便于对账。
func toolRef(callID, itemID string) string {
	if callID != "" {
		return callID
	}
	return itemRef(itemID)
}
