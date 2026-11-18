// Package compose 是记忆块拼装（门面私有）：把会话历史与经验四节拼成消息链。
// 它不属于共享库——experience 只负责产生节，不负责插入位置与裁剪。
package compose

import (
	"strings"

	"github.com/NouSart/sump/protocol"
)

// MemoryPrefix 是记忆消息的内容前缀（用于识别并替换自己的旧注入）。
const MemoryPrefix = "【记忆】"

// ConversationTail 提取任务上下文尾部的对话消息（user/assistant 文本），
// 遇到第一条 system（system prompt 或旧记忆块）即停——那是任务链的边界。
func ConversationTail(messages []protocol.Message) []protocol.Message {
	var tail []protocol.Message
	for i := len(messages) - 1; i >= 0; i-- {
		m := messages[i]
		if m.Role == "system" {
			break
		}
		if isDialogText(m) {
			tail = append(tail, m)
		}
	}
	for i, j := 0, len(tail)-1; i < j; i, j = i+1, j-1 {
		tail[i], tail[j] = tail[j], tail[i]
	}
	return tail
}

func isDialogText(m protocol.Message) bool {
	return (m.Role == "user" || m.Role == "assistant") &&
		strings.TrimSpace(m.Content) != "" && len(m.ToolCalls) == 0
}

// TrimOverlap 从 history 尾部裁去与 tail 尾部逐条相同（role+content）的部分。
func TrimOverlap(history, tail []protocol.Message) []protocol.Message {
	i, j := len(history)-1, len(tail)-1
	for i >= 0 && j >= 0 {
		if history[i].Role != tail[j].Role || history[i].Content != tail[j].Content {
			break
		}
		i--
		j--
	}
	return history[:i+1]
}

// BuildText 把历史消息拼成“用户: … / 助手: …”的记忆文本。
func BuildText(history []protocol.Message) string {
	var b strings.Builder
	for _, m := range history {
		switch m.Role {
		case "user":
			b.WriteString("用户: ")
		case "assistant":
			b.WriteString("助手: ")
		default:
			continue
		}
		b.WriteString(strings.TrimSpace(m.Content))
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}

// BuildMemory 组装记忆文本：最近对话 + 核心记忆 + 相关记忆 + 经验线头 + 其他会话。
func BuildMemory(history []protocol.Message, core, relevant, experience, others []string) string {
	var sections []string
	if text := BuildText(history); text != "" {
		sections = append(sections, "（最近对话）\n"+text)
	}
	if len(core) > 0 {
		sections = append(sections, "（核心记忆）\n"+strings.Join(core, "\n"))
	}
	if len(relevant) > 0 {
		sections = append(sections, "（相关记忆）\n"+strings.Join(relevant, "\n"))
	}
	if len(experience) > 0 {
		sections = append(sections, "（经验线头）\n"+
			"以下是以前干过的同类事，每条一行；想深挖就调 memory__dig（按 id）。\n"+
			strings.Join(experience, "\n"))
	}
	if len(others) > 0 {
		sections = append(sections, "（其他会话）\n"+
			"这些是别的会话**在做什么**的摘要；你看不到它们的对话、工具输出与文件。\n"+
			strings.Join(others, "\n"))
	}
	return strings.Join(sections, "\n\n")
}

// Inject 将记忆文本注入消息链：替换已有记忆块，否则插到第一条 system 之后。
func Inject(messages []protocol.Message, memoryText string) []protocol.Message {
	out := make([]protocol.Message, 0, len(messages)+1)
	placed := false
	for _, m := range messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, MemoryPrefix) {
			if memoryText != "" && !placed {
				out = append(out, protocol.Message{Role: "system", Content: MemoryPrefix + "\n" + memoryText})
				placed = true
			}
			continue
		}
		out = append(out, m)
		if memoryText != "" && !placed && m.Role == "system" {
			out = append(out, protocol.Message{Role: "system", Content: MemoryPrefix + "\n" + memoryText})
			placed = true
		}
	}
	if memoryText != "" && !placed {
		out = append([]protocol.Message{{Role: "system", Content: MemoryPrefix + "\n" + memoryText}}, out...)
	}
	return out
}
