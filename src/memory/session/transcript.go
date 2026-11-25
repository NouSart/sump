package session

// transcript（S-001 §3.2，A 切片）：把一份已组装好的上下文快照渲染成可读文本。
//
// 与 context_v2 共用读取 / 构建代码：同一个 protocol.ContextResponse 投影、同一个
// LoadSnapshotByID（同一份 gzip 解压与过期判定）。本文件不重新查 messages、不重新组装、
// 不改触发状态、不落任何东西——只读，天然幂等。
//
// S-001 §3.2 只写了「{work, snapshot_id, budget{}} → {text, truncated, omitted}」与
// 「与 context 共用读取/构建代码；带 snapshot_id 时要求与该快照一致」，没有定义 budget
// 形状与文本格式。这里的默认口径（同时写进 session/transcript_test.go）：
//
//   - snapshot_id 必填：文本必须对应调用方点名的那份投影，服务端不猜「这个 work 的最新快照」；
//   - budget.max_chars 按字符（rune）计，0 = 不额外限制；无论给不给预算，输出文本都有
//     1 MiB（1048576 字节）硬顶，避免一次读快照把调用方撑爆；负数非法；
//   - 文本按消息顺序逐条渲染成 "role: content"，用换行分隔；tool 消息在末尾附
//     " [tool_call_id=<id>]"（有 id 时），让工具调用与它的返回可配对；
//   - truncated=true 表示文本末尾被预算截断（可能是某条被截成前缀，也可能是整条起就放不下）；
//   - omitted 只统计「因预算整条丢弃」的消息条数：被截成前缀的那条不算整条丢弃。
//
// 错误映射：body / work / snapshot_id / budget 非法 → invalid_argument；越权 work →
// permission_denied（先过 authorizeTargetWork）；work / snapshot 不存在、过期，或快照属于
// 别的 work → not_found；解压 / 解码失败 → internal。

import (
	"context"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// transcriptHardCapBytes 是输出文本的硬顶（1 MiB）。预算只能往下压，不能往上抬。
const transcriptHardCapBytes = 1 << 20

// registerTranscript 注册 transcript（在 registerNative 里调用）。
// 不是工具（Tool=false）：模型不该自己决定「历史要按什么格式重放一遍」。
func (s *Server) registerTranscript() {
	s.HandleNative(transcriptMethod(), s.nativeTranscript)
}

// transcriptMethod 是 transcript 的服务卡声明（独立成函数，测试可直接校验 schema）。
func transcriptMethod() spv2.Method {
	return spv2.Method{
		Name:        "transcript",
		Description: "把指定快照渲染成文本（只读；与 context_v2 共用读取代码）",
		Input: objectSchema(map[string]any{
			"work":        stringSchema(),
			"snapshot_id": stringSchema(),
			"budget": objectSchema(map[string]any{
				"max_chars": integerSchema(),
			}),
		}, "work", "snapshot_id"),
		Output: objectSchema(map[string]any{
			"text":      stringSchema(),
			"truncated": booleanSchema(),
			"omitted":   integerSchema(),
		}, "text", "truncated", "omitted"),
		Tool: false,
	}
}

// nativeTranscript 是原生入口：只做「解码 body → 共享实现 → 错误码」。
func (s *Server) nativeTranscript(ctx context.Context, call *spv2.Call) error {
	out, err := s.transcript(ctx, call.Frame.Body, callerOf(call))
	if err != nil {
		return replyContextV2Error(call, err)
	}
	return call.Reply(out)
}

// transcript 是 transcript 的共享实现。
func (s *Server) transcript(ctx context.Context, body []byte, c caller) (any, error) {
	var req protocol.TranscriptPayload
	if err := protocol.DecodeRaw(body, &req); err != nil {
		return nil, v2Error(spv2.CodeInvalidArgument, "transcript body 无效: %v", err)
	}
	work := strings.TrimSpace(req.Work)
	snapshotID := strings.TrimSpace(req.SnapshotID)
	if work == "" {
		return nil, v2Error(spv2.CodeInvalidArgument, "transcript: work 必填")
	}
	if snapshotID == "" {
		return nil, v2Error(spv2.CodeInvalidArgument, "transcript: snapshot_id 必填（不猜「最新一份快照」）")
	}
	if req.Budget.MaxChars < 0 {
		return nil, v2Error(spv2.CodeInvalidArgument,
			"transcript: budget.max_chars 不能为负（收到 %d）", req.Budget.MaxChars)
	}
	// 授权在读快照之前：越权请求不能借「快照在不在」的差异探测别人的 work。
	if _, err := s.authorizeTargetWork(ctx, work, c.work, "transcript", c.from); err != nil {
		return nil, err
	}
	_, payload, _, err := s.cfg.Store.LoadSnapshotByID(ctx, work, snapshotID)
	switch {
	case errors.Is(err, store.ErrSnapshotNotFound):
		// 不存在与「属于别的 work」同形：归属条件在 SELECT 里，不在这里补一次回显。
		return nil, v2Error(spv2.CodeNotFound,
			"transcript: work %s 下没有快照 %s", work, snapshotID)
	case errors.Is(err, store.ErrSnapshotExpired):
		return nil, v2Error(spv2.CodeNotFound,
			"transcript: 快照 %s 已过期，不能当作完整历史重放", snapshotID)
	case err != nil:
		return nil, v2Error(spv2.CodeInternal, "transcript: %v", err)
	}
	var snap protocol.ContextResponse
	if err := protocol.DecodeRaw(payload, &snap); err != nil {
		return nil, v2Error(spv2.CodeInternal,
			"transcript: 快照 %s 载荷无法解析: %v", snapshotID, err)
	}
	return renderTranscript(snap.Messages, req.Budget.MaxChars), nil
}

// renderTranscript 把快照里的消息链渲染成文本并按预算裁剪。
//
// 裁剪只从末尾丢：消息顺序就是历史顺序，中间挖洞会让「工具调用 / 工具结果」对不上。
// 预留换行符也算进预算——返回值里的 Text 保证 rune 数不超过预算、字节数不超过硬顶。
func renderTranscript(msgs []protocol.LLMMessage, maxChars int) protocol.TranscriptResult {
	limitRunes, limitBytes := transcriptLimits(maxChars)

	var b strings.Builder
	usedRunes, usedBytes, kept := 0, 0, 0
	for _, m := range msgs {
		line := transcriptLine(m)
		sep := ""
		if kept > 0 {
			sep = "\n"
		}
		needRunes := utf8.RuneCountInString(sep) + utf8.RuneCountInString(line)
		needBytes := len(sep) + len(line)
		if usedRunes+needRunes <= limitRunes && usedBytes+needBytes <= limitBytes {
			b.WriteString(sep)
			b.WriteString(line)
			usedRunes += needRunes
			usedBytes += needBytes
			kept++
			continue
		}
		// 这一条放不下整条：能放多少放多少（可能是空），后面的一律整条丢弃。
		roomRunes := limitRunes - usedRunes - utf8.RuneCountInString(sep)
		roomBytes := limitBytes - usedBytes - len(sep)
		prefix := clipToBudget(line, roomRunes, roomBytes)
		if prefix == "" {
			return protocol.TranscriptResult{Text: b.String(), Truncated: true, Omitted: len(msgs) - kept}
		}
		b.WriteString(sep)
		b.WriteString(prefix)
		// 这一条以「前缀」的形式留下了，不算整条丢弃。
		return protocol.TranscriptResult{Text: b.String(), Truncated: true, Omitted: len(msgs) - kept - 1}
	}
	return protocol.TranscriptResult{Text: b.String()}
}

// transcriptLimits 返回本次渲染的 (rune 上限, 字节硬顶)。
// max_chars<=0 表示不额外限制，此时 rune 上限退到硬顶本身（真正的边界仍是字节数）。
func transcriptLimits(maxChars int) (limitRunes, limitBytes int) {
	limitRunes = maxChars
	if limitRunes <= 0 || limitRunes > transcriptHardCapBytes {
		limitRunes = transcriptHardCapBytes
	}
	return limitRunes, transcriptHardCapBytes
}

// transcriptLine 渲染一条消息：`role: content`；tool 消息附 tool_call_id。
// 空内容也保留 role 前缀：一条空消息在历史里是事实，不该被悄悄抹掉。
func transcriptLine(m protocol.LLMMessage) string {
	line := m.Role + ": " + m.Content
	if m.Role == "tool" && m.ToolCallID != "" {
		line += " [tool_call_id=" + m.ToolCallID + "]"
	}
	return line
}

// clipToBudget 返回 s 的最长前缀，使它的 rune 数与字节数都不超过给定余量。
// 按 rune 切（不按字节）：中文被拦腰砍成半个字会变成乱码。
func clipToBudget(s string, maxRunes, maxBytes int) string {
	if maxRunes <= 0 || maxBytes <= 0 {
		return ""
	}
	runes := 0
	for i := range s {
		_, size := utf8.DecodeRuneInString(s[i:])
		if runes+1 > maxRunes || i+size > maxBytes {
			return s[:i]
		}
		runes++
	}
	return s
}
