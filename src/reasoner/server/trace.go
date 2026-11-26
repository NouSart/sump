// 会话轨迹：把"思考 / 终端命令 / 终端输出"写进记忆，并顺手推给能显示的入口方。
//
// 记忆是真相（历史回填、跨端一致）；progress 只是让界面不必等到最终答复。
// 写记忆走同步 Call：载荷带 Work 与 ClientItemID，回执里的 item_id 才能被调用方记账
// （用户消息的 item_id 就是本轮 context 的 trigger.item）。progress 仍走 Fire：
// 它是展示面，失败只记日志，不影响推理本身。
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

// recordMaxChars 是单条轨迹记录的落库上限：终端输出上限是 256KB，
// 原样存会把库和界面一起撑爆（超出部分标 truncated + 原始字数）。
const recordMaxChars = 8000

// 记录类型（与门户的渲染分支一一对应）。
const (
	kindReasoning  = "reasoning"
	kindToolCall   = "tool_call"
	kindToolResult = "tool_result"
)

// nextSeq 返回会话内单调序号：Fire 是 goroutine，落地顺序不保证，
// 但 seq 保证"命令在输出之前"这种真实先后。
// 用**微秒时间戳**而不是进程内计数器：reasoner 重启后计数器会从头开始，
// 那样新记录会排到旧记录前面。不用纳秒是因为门户要经 JSON 读它：
// JS 的 number 只能精确表示 2^53（约 9e15），纳秒级 1.8e18 会被抹平，
// 相邻记录的序号可能撞成同一个数（去重就会误杀）。微秒级 1.8e15 仍在安全区。
func (s *Server) nextSeq() int64 {
	for {
		next := time.Now().UnixMicro()
		last := atomic.LoadInt64(&s.memSeq)
		if next <= last {
			next = last + 1
		}
		if atomic.CompareAndSwapInt64(&s.memSeq, last, next) {
			return next
		}
	}
}

// clampRecord 截断过长的记录（按字符切，不切坏 UTF-8）。
func clampRecord(text string) (string, bool) {
	runes := []rune(text)
	if len(runes) <= recordMaxChars {
		return text, false
	}
	return string(runes[:recordMaxChars]), true
}

// record 落一条会话记录：写记忆 + （入口方声明了 progress 时）推一条实况。
// meta 只放**给人看**的附加信息（工具名、命令、call_id…），不放正文。
// 返回 store 回执里的 item_id；落库失败时返回错误（调用方决定是否中断）。
func (s *Server) record(cid, boss, trace, taskID, kind, role, content string, meta map[string]any) (string, error) {
	return s.recordNamed(cid, boss, trace, taskID, kind, role, content, meta)
}

// recordTask 与 record 相同，只为语义命名保留；任务名已由 work_open 承担
// （StorePayload.task_name 在 P2 后无读者，发送方一律不填）。
func (s *Server) recordTask(cid, boss, trace, taskID, kind, role, content string, meta map[string]any) (string, error) {
	return s.recordNamed(cid, boss, trace, taskID, kind, role, content, meta)
}

func (s *Server) recordNamed(cid, boss, trace, taskID, kind, role, content string, meta map[string]any) (string, error) {
	if strings.TrimSpace(content) == "" {
		return "", nil
	}
	clamped, truncated := clampRecord(content)
	metaJSON := ""
	if len(meta) > 0 || truncated {
		m := make(map[string]any, len(meta)+2)
		for k, v := range meta {
			m[k] = v
		}
		if truncated {
			m["truncated"] = true
			m["full_chars"] = len([]rune(content))
		}
		if raw, err := json.Marshal(m); err == nil {
			metaJSON = string(raw)
		}
	}

	seq := s.nextSeq()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	data, err := s.Call(ctx, s.memoryName(), "store", protocol.StorePayload{
		ConversationID: cid, Work: trace, TaskID: taskID, Boss: boss, Role: role,
		Content: clamped, Kind: kind, Meta: metaJSON, Seq: seq,
		ClientItemID: s.clientItemID(boss, kind),
	}, trace)
	if err != nil {
		s.logger.Printf("会话 %s：store(%s/%s) 失败: %v", cid, kind, role, err)
		return "", fmt.Errorf("store(%s/%s) 失败: %w", kind, role, err)
	}
	var res protocol.StoreResult
	if err := protocol.DecodeRaw(data, &res); err != nil {
		s.logger.Printf("会话 %s：store(%s/%s) 回执无法解码: %v", cid, kind, role, err)
		return "", fmt.Errorf("store(%s/%s) 回执无法解码: %w", kind, role, err)
	}
	if s.bossWantsProgress(boss) {
		s.Fire(boss, "progress", protocol.ProgressPayload{
			ConversationID: cid, TaskID: taskID, Seq: seq, Kind: kind, Content: clamped, Meta: metaJSON,
			At: time.Now().Format(time.RFC3339Nano),
		}, trace)
	}
	return res.ItemID, nil
}

// clientItemID 生成 store 幂等键（S-001 §3.0 规则 5）：
//   - 用户 / 助手消息：<entry_service>:<entry_item_id>；reasoner 代入口写时 entry_item_id
//     由 reasoner 生成（同一个任务内的每条记录各不相同）；
//   - 其它（reasoning / tool_call / tool_result）：调用方生成的 ULID。
//
// boss 为空或 "self" 时 entry 记 reasoner 自己（结果归属方语义）。
func (s *Server) clientItemID(boss, kind string) string {
	if kind == "user" || kind == "assistant" {
		entry := boss
		if entry == "" || entry == "self" {
			entry = s.cfg.Name
		}
		return entry + ":" + spv2.NewID()
	}
	return spv2.NewID()
}

// bossWantsProgress 判断入口方要不要实况：**只认它自己在 provides 里声明过的 progress**。
// 没声明（比如 QQ）就不推——否则对方只会回一个 unknown action，白报一次失败。
func (s *Server) bossWantsProgress(boss string) bool {
	if boss == "" || boss == "self" {
		return false
	}
	for _, card := range s.Roster() {
		if card.Name != boss {
			continue
		}
		for _, p := range card.Provides {
			if p.Action == "progress" {
				return true
			}
		}
		return false
	}
	return false
}

// toolCallTrace 把一次工具调用变成"给人看的命令"：
// 带 command 参数的工具（shell）直接拎出来，其余工具显示 工具名 + 参数原文。
func toolCallTrace(call llm.ToolCall) (string, map[string]any) {
	name := call.Function.Name
	args := strings.TrimSpace(call.Function.Arguments)
	meta := map[string]any{"tool": name, "call_id": call.ID}
	if args != "" {
		var parsed struct {
			Command string `json:"command"`
		}
		if err := json.Unmarshal([]byte(args), &parsed); err == nil && strings.TrimSpace(parsed.Command) != "" {
			return parsed.Command, meta
		}
		return name + " " + args, meta
	}
	return name, meta
}
