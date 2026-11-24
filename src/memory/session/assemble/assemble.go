package assemble

import (
	"fmt"
	"sort"
	"strings"
)

// Assemble 把一份快照组装成可直接交给 LLM 的 messages（memory 是唯一组装者）。
//
// 消息顺序固定（前缀稳定，见 S-001 INV-1/INV-3 与 R-011 §3 的 assemble 测试面）：
//  1. system：任务提示 + 记忆块（核心记忆在前，相关记忆/经验/其他会话在后）；
//  2. 历史对话（按 Seq 升序，窗口内 user/assistant）；
//  3. 图片索引行（role=user，只带 gist，不内联字节）；
//  4. 工具配对块（逐个 call：assistant.tool_calls → role=tool 结果/占位）。
//
// 稳定块（任务提示、核心记忆）永远在前，易变块按固定顺序排在后面，所以“只改历史
// 尾部”的两次组装共享完全相同的 system 前缀（可安全做前缀缓存/双跑比对）。
//
// 错误语义：组装永不因依赖缺失而失败（见 sump-v2-memory-decisions.md“assemble 永不
// 因依赖失败而报错”）。未知结果、孤立结果、截断、超窗口都写 warnings；degraded
// 原样透传。error 目前恒为 nil，留给将来的输入校验。
func Assemble(in Input) (Output, error) {
	b := &builder{
		in:     in,
		cfg:    in.Config,
		grants: messageGrants(in.Grants),
		out:    &Output{Degraded: cloneDegraded(in.Degraded)},
	}
	// 1) 可见性：只让“本 work / 显式 task·thread / 显式授权”的记录进入候选。
	//    被过滤的记录不进 messages，只写 provenance（source=filtered）当证据。
	history := b.visible(in.History)
	calls := b.visible(in.ToolCalls)
	results := b.visible(in.ToolResults)

	// 2) 显式稳定排序：按 Seq 升序；同一 Seq 保持调用方给的顺序（切片是副本，不改 Input）。
	sortBySeq(history)
	sortBySeq(calls)
	sortBySeq(results)

	// 3) 历史窗口：超窗口从最老丢，并 warning（既有证据也有提示）。
	history = b.window(history)

	// 4) system 块永远在前（哪怕内容为空）：模型输入的形态从第一条就确定。
	b.out.Messages = append(b.out.Messages, Message{Role: "system"})
	b.appendSystem()

	// 5) 易变块按固定顺序：历史 → 图片 → 工具结果。
	b.appendHistory(history)
	b.appendImages()
	b.appendTools(calls, results)

	// provenance 按 messages 下标重排（-1 的排除类证据统一放末尾），输出稳定。
	sortProvenance(b.out.Provenance)
	return *b.out, nil
}

// builder 是一次组装的内部状态：只写 out，不改 in。
type builder struct {
	in     Input
	cfg    Config
	grants map[string]bool // 显式授权的 ref（message id 或 call id）
	out    *Output
}

// msg 追加一条文本消息并返回它在 Output.Messages 里的下标。
func (b *builder) msg(role, content string) int {
	idx := len(b.out.Messages)
	b.out.Messages = append(b.out.Messages, Message{Role: role, Content: content})
	return idx
}

// toolMsg 追加一条 role=tool 消息（占住对应 tool_call 的配对位）。
func (b *builder) toolMsg(content, callID string) int {
	idx := len(b.out.Messages)
	b.out.Messages = append(b.out.Messages, Message{Role: "tool", Content: content, ToolCallID: callID})
	return idx
}

// prov 记一条来源证据。
func (b *builder) prov(index int, source, ref string) {
	b.out.Provenance = append(b.out.Provenance, Provenance{Index: index, Source: source, Ref: ref})
}

// warnf 记一条 warning（对象、数字都写清楚，便于事后定位）。
func (b *builder) warnf(format string, args ...any) {
	b.out.Warnings = append(b.out.Warnings, fmt.Sprintf(format, args...))
}

// ---------- 可见性（S-001 §5.2 / INV-14） ----------

// visible 过滤不可见记录：同 work 默认可见；task/thread 由写入方显式发布，可见；
// 其余必须命中 Grants 里的显式 ref。过滤掉的只写 provenance，不进 messages。
//
// 注意：本函数拿到的就是“本 work 的快照”，所以 task 级记录默认可见是对的——
// 跨 task 的收窄应该在投影层完成；thread 同理（thread 场景由调用方决定是否投影）。
func (b *builder) visible(records []Record) []Record {
	if len(records) == 0 {
		return nil
	}
	out := make([]Record, 0, len(records))
	for _, r := range records {
		if recordVisible(r, b.in.WorkID, b.grants) {
			out = append(out, r)
			continue
		}
		// 默认不可见：不给内容，只留“这条被过滤了”的事实（INV-4 无静默失败）。
		b.prov(-1, "filtered", r.ItemID)
	}
	return out
}

// recordVisible 是可见性的唯一判据。
func recordVisible(r Record, workID string, grants map[string]bool) bool {
	if r.WorkID == workID {
		return true // 自己的 work（含两者都为空的历史测试数据/单机场景）
	}
	switch r.Visibility {
	case "task", "thread":
		return true // 写入方显式发布过可见范围
	}
	if grants[r.ItemID] {
		return true // 显式授予的消息
	}
	if r.CallID != "" && grants[r.CallID] {
		return true // 显式授予的调用（结果与调用一起可见）
	}
	return false // 默认不可见（INV-14）
}

// messageGrants 把 Grants 收成查表集合。RefKind 只区分命名空间：只有 message
// （或空，老数据）授权会作用于消息/调用；snapshot/memory 授权不在这里展开。
func messageGrants(grants []Grant) map[string]bool {
	if len(grants) == 0 {
		return nil
	}
	m := make(map[string]bool, len(grants))
	for _, g := range grants {
		if g.RefID == "" {
			continue
		}
		if g.RefKind != "" && g.RefKind != "message" {
			continue
		}
		m[g.RefID] = true
	}
	return m
}

// ---------- 历史窗口 ----------

// window 只保留最近 HistoryLimit 条（>0 时）；丢掉的最旧记录写 provenance 并 warning。
func (b *builder) window(history []Record) []Record {
	limit := b.cfg.HistoryLimit
	if limit <= 0 || len(history) <= limit {
		return history
	}
	dropped := len(history) - limit
	for _, r := range history[:dropped] {
		b.prov(-1, "window", r.ItemID)
	}
	b.warnf("历史超出窗口：丢弃最旧的 %d 条（窗口 %d）", dropped, limit)
	return history[dropped:]
}

// ---------- system ----------

// appendSystem 渲染固定前缀：任务提示 + 记忆块。
func (b *builder) appendSystem() {
	var parts []string
	if prompt := taskPrompt(b.in.TaskName, b.in.TaskID); prompt != "" {
		parts = append(parts, prompt)
		ref := "task"
		if b.in.TaskID != "" {
			ref = "task:" + b.in.TaskID
		}
		b.prov(0, "system", ref)
	}
	if block, desc := b.memoryBlock(); block != "" {
		parts = append(parts, MemoryPrefix+"\n"+block)
		b.prov(0, "memory", desc)
	}
	b.out.Messages[0].Content = strings.Join(parts, "\n\n")
}

// ---------- 历史消息 ----------

// appendHistory 把窗口内的历史记录渲染成对话消息。
// 规则：kind=reasoning 不进 messages（可进 provenance）；只有 user/assistant 能渲染；
// 其余（system / tool_* 错放）排除并 warning——不允许静默吞掉一条记录。
func (b *builder) appendHistory(history []Record) {
	for _, r := range history {
		kind := r.Kind
		if kind == "" {
			kind = kindOfRole(r.Role)
		}
		if kind == "reasoning" {
			// 思考回灌纯浪费额度，某些模型还会直接拒；排除是设计，不是失败。
			b.prov(-1, "excluded", r.ItemID)
			continue
		}
		role := r.Role
		if role == "" {
			role = roleOfKind(kind)
		}
		switch {
		case role == "user" || role == "assistant":
			if kind == "tool_call" || kind == "tool_result" {
				// 工具记录必须走 ToolCalls/ToolResults 才能配对；错放在 History 里
				// 回灌会造出“没有 tool_calls 的 tool 消息”，上游会拒。
				b.warnf("历史记录 %s 的 kind=%s 应放在 ToolCalls/ToolResults 里，已排除", itemRef(r.ItemID), kind)
				b.prov(-1, "excluded", r.ItemID)
				continue
			}
			idx := b.msg(role, r.Content)
			b.prov(idx, "history", r.ItemID)
		case role == "system":
			b.warnf("历史记录 %s 是 system，已排除（系统提示只能由 memory 组装）", itemRef(r.ItemID))
			b.prov(-1, "excluded", r.ItemID)
		default:
			b.warnf("历史记录 %s 的 role=%q 不能渲染成对话消息，已排除", itemRef(r.ItemID), role)
			b.prov(-1, "excluded", r.ItemID)
		}
	}
}

// ---------- 图片 ----------

// appendImages 把图片索引渲染成文本行（纯函数不取字节，不内联 base64）。
// 首次看到的图带 id（首次才需要“模型看见过这张图”的锚点）；之后只留 gist 行。
func (b *builder) appendImages() {
	if !b.cfg.IncludeImages || len(b.in.Images) == 0 {
		return
	}
	for _, img := range b.in.Images {
		idx := b.msg("user", imageLine(img))
		b.prov(idx, "image", img.ID)
	}
}

// imageLine 是单张图的展示行：
//   - 首次：[图片 <id>：<gist>]
//   - 之后：[图片：<gist>]（只留 gist 行，不重复 id）
//   - lost：括号内追加（原图已丢失）——语义是“有印象但想不起细节”，不是错误。
func imageLine(img ImageRef) string {
	gist := strings.TrimSpace(img.Gist)
	if gist == "" {
		gist = "未记录"
	}
	var sb strings.Builder
	if img.First && img.ID != "" {
		fmt.Fprintf(&sb, "[图片 %s：%s", img.ID, gist)
	} else {
		fmt.Fprintf(&sb, "[图片：%s", gist)
	}
	if img.Status == "lost" {
		sb.WriteString("（原图已丢失）")
	}
	sb.WriteString("]")
	return sb.String()
}

// ---------- 排序与工具函数 ----------

// sortBySeq 按 Seq 升序稳定排序（同 Seq 保持调用方给的相对顺序）。
func sortBySeq(records []Record) {
	sort.SliceStable(records, func(i, j int) bool { return records[i].Seq < records[j].Seq })
}

// sortProvenance 把“已进入 messages”的证据按下标排前面，排除类（-1）放末尾；
// 同下标保持写入顺序（系统块里 task 在 memory 前）。
func sortProvenance(prov []Provenance) {
	sort.SliceStable(prov, func(i, j int) bool {
		a, b := prov[i], prov[j]
		if (a.Index >= 0) != (b.Index >= 0) {
			return a.Index >= 0 // 有下标的排前面
		}
		if a.Index >= 0 {
			return a.Index < b.Index
		}
		return false // 都是 -1：保持写入顺序
	})
}

// kindOfRole 在 kind 缺失时按 role 推断（与 store.kindOfRole 同口径）。
func kindOfRole(role string) string {
	switch role {
	case "user":
		return "user"
	case "assistant":
		return "assistant"
	case "tool":
		return "tool_result"
	case "system":
		return "system"
	default:
		return ""
	}
}

// roleOfKind 在 role 缺失时按 kind 推断（spawn_return 是子 work 回传的一轮输入，
// 按用户消息渲染；tool_call 的承载体按 assistant 渲染）。
func roleOfKind(kind string) string {
	switch kind {
	case "user", "spawn_return":
		return "user"
	case "assistant":
		return "assistant"
	case "tool_call":
		return "assistant"
	case "tool_result":
		return "tool"
	case "reasoning":
		return "assistant"
	default:
		return ""
	}
}

// itemRef 把可能为空的记录 id 变成可读的提示文字（warning 里不出现空对象）。
func itemRef(id string) string {
	if id == "" {
		return "（无 id）"
	}
	return id
}

// cloneDegraded 深拷贝降级状态：Output 与调用方之后各自改动互不影响。
func cloneDegraded(d *Degraded) *Degraded {
	if d == nil {
		return nil
	}
	cp := *d
	return &cp
}
