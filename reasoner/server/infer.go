package server

// 单步推理：roster → LLM 工具、工具派发（异步）与交付（deliver）。
// 完全启动链：任务入口先经记忆服务（recall 托管上下文）；只有“from=memory 的 resume”
// 才触发对 LLM API 的调用。任务上下文与在跑工具由内存黑板持有（见 board.go）：
// 工具调用即时回执、结果注入黑板再唤醒下一轮；服务不驻留循环。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// systemPrompt 是单步推理器的系统提示（M5 异步工具版）。
const systemPrompt = `你是 SUMP 服务网络中的“单步推理器”（reasoner 服务）。
你会收到一段对话上下文。你的职责是决定下一步：
1. 需要外部能力时，调用提供的工具（工具 = 网络中其他服务的动作）；
2. 当任务可以收尾、或用户问候闲聊时，直接输出最终答复文本。

工具是异步执行的：
- 你发起调用后会立刻收到“已受理”回执（tool 消息），执行结果稍后以“[工具结果] …”开头的消息送达；
- 可以并行调用多个工具；也可以调用 wait 工具等待全部工具完成，或先继续做其他事；
- 仍有工具任务未完成时不要收尾——若提前输出最终答复，系统会拒绝并要求你继续等待。`

// structuralActions 是即便声明 tool:true 也不作为工具暴露的动作：协议/链机制动作（模型不得绕过链路），
// 以及本服务的调试/测试动作（echo / debug_jump——不给模型“万能遥控”）。
// 两道闸共用这张表：rosterTools 生成列表时过滤（暴露面）；runToolCall 执行前同样校验并拒绝（执行面）——
// 模型若仍指名调用未提供的动作（幻觉 / 注入），不会发生真实跳转。
var structuralActions = map[string]bool{
	"user_message": true,
	"step":         true,
	"deliver":      true,
	"recall":       true, // memory 服务：记忆请求
	"store":        true, // memory 服务：对话落库
	"resume":       true, // 本服务的“完全启动”入口（仅接受 from=memory）
	"save":         true, // images 服务：保存图片（qq 侧调用）
	"fetch":        true, // images 服务：取图（llm 侧按需调用）
	"echo":         true, // 本服务：连通性测试（仅供直连调试）
	"debug_jump":   true, // 本服务：调试代理跳转
	"configure":    true, // 骨架保留动作：设置下发（中心驱动，不进 provides）
}

// rosterTools 把名册里各服务**显式声明 tool:true** 的动作映射为 LLM 工具
// （未声明的不暴露；保留 / 调试动作再由 structuralActions 拦下——暴露与执行两侧同一判据）。
func (s *Server) rosterTools() []llm.Tool {
	var out []llm.Tool
	for _, card := range s.Roster() {
		for _, p := range card.Provides {
			if !p.Tool || structuralActions[p.Action] {
				continue
			}
			out = append(out, llm.Tool{
				Type: "function",
				Function: llm.ToolFunction{
					Name:        card.Name + "__" + p.Action,
					Description: fmt.Sprintf("服务 %s（%s）的动作 %s：输入 %s；输出 %s", card.Name, card.Description, p.Action, p.Input, p.Output),
					Parameters:  map[string]any{"type": "object"},
				},
			})
		}
	}
	// 内置工具（不来自名册）：wait——等待本会话全部工具任务完成。
	out = append(out, llm.Tool{
		Type: "function",
		Function: llm.ToolFunction{
			Name:        waitToolName,
			Description: "等待本会话所有进行中的工具任务完成（阻塞到结果到齐或超时）。结果尚未到齐、不能收尾时使用。",
			Parameters:  map[string]any{"type": "object"},
		},
	})
	return out
}

// parseToolName 解析“服务__动作”。
func parseToolName(name string) (service, action string, ok bool) {
	parts := strings.SplitN(name, "__", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// argsToRaw 把工具调用的 JSON 参数转为协议的 msgpack 输入。
func argsToRaw(args string) (msgpack.RawMessage, error) {
	if strings.TrimSpace(args) == "" {
		return nil, nil
	}
	var v any
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return nil, fmt.Errorf("参数不是合法 JSON: %w", err)
	}
	return protocol.EncodePayload(v)
}

// rawToJSONText 把 msgpack 原始数据转为 JSON 文本（给 LLM 阅读）。
func rawToJSONText(raw msgpack.RawMessage) string {
	if len(raw) == 0 {
		return "{}"
	}
	var v any
	if err := msgpack.Unmarshal(raw, &v); err != nil {
		return fmt.Sprintf("%v", []byte(raw))
	}
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// toolAdvertised 判断某服务动作当前是否为“对 LLM 提供”的工具（provides 声明 tool:true 且非保留动作）。
// 与 rosterTools 同一把尺子——执行侧复查，用于拒绝指名调用未提供的动作。
func (s *Server) toolAdvertised(service, action string) bool {
	if structuralActions[action] {
		return false
	}
	for _, card := range s.Roster() {
		if card.Name != service {
			continue
		}
		for _, p := range card.Provides {
			if p.Action == action && p.Tool {
				return true
			}
		}
		return false
	}
	return false
}

// runToolCall 执行一次工具调用并返回给 LLM 的内容文本（JSON）。
func (s *Server) runToolCall(ctx context.Context, trace string, call llm.ToolCall) (string, error) {
	svc, action, ok := parseToolName(call.Function.Name)
	if !ok {
		return "", fmt.Errorf("工具名无效: %s", call.Function.Name)
	}
	if !s.toolAdvertised(svc, action) {
		return "", fmt.Errorf("工具不可用（未提供或属保留动作）: %s", call.Function.Name)
	}
	rawIn, err := argsToRaw(call.Function.Arguments)
	if err != nil {
		return "", err
	}
	data, err := s.Call(ctx, svc, action, rawIn, trace, "")
	if err != nil {
		return "", err
	}
	return rawToJSONText(data), nil
}

// toolSem 返回某会话的工具并发信号量（nil = 不限制）。信号量懒创建、按会话复用；
// 并发上限的变更只影响此后新建的会话（已建信号量不缩容）。
func (s *Server) toolSem(conversationID string) chan struct{} {
	n := s.toolConcurrency()
	if n < 0 {
		return nil
	}
	if n == 0 {
		n = 10
	}
	s.semsMu.Lock()
	defer s.semsMu.Unlock()
	sem := s.sems[conversationID]
	if sem == nil {
		sem = make(chan struct{}, n)
		s.sems[conversationID] = sem
	}
	return sem
}

// resolveImages 取“最近一条带图消息”的图片数据（id → data URL），供 LLM 内联。
// 更早的图片引用保持文本占位（由 llm 序列化层处理）；失败仅记日志，不阻断推理。
func (s *Server) resolveImages(ctx context.Context, trace string, messages []llm.Message) map[string]string {
	var ids []string
	for i := len(messages) - 1; i >= 0; i-- {
		if len(messages[i].ImageIDs) > 0 {
			ids = messages[i].ImageIDs
			break
		}
	}
	if len(ids) == 0 {
		return nil
	}
	if _, _, err := s.Resolve(s.cfg.Images); err != nil {
		s.logger.Printf("取图失败（服务 %s）: %v", s.cfg.Images, err)
		return nil
	}
	out := make(map[string]string, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			data, err := s.Call(ctx, s.cfg.Images, "fetch", protocol.ImageFetchPayload{ID: id}, trace, "")
			if err != nil {
				s.logger.Printf("取图 %s 失败: %v", id, err)
				return
			}
			var fr protocol.ImageFetchResult
			if err := protocol.DecodeRaw(data, &fr); err != nil || fr.Data == "" {
				s.logger.Printf("取图 %s 结果无效", id)
				return
			}
			mu.Lock()
			out[id] = "data:" + fr.Mime + ";base64," + fr.Data
			mu.Unlock()
		}(id)
	}
	wg.Wait()
	if len(out) > 0 {
		s.logger.Printf("已取图 %d/%d 张（内联给模型）", len(out), len(ids))
	}
	return out
}

// startMemoryRound 开启一轮“记忆往返”：把任务上下文（含原 boss）托管给记忆服务。
// 记忆服务的 resume（from=memory）回来时才真正调用 LLM API（完全启动）。
func (s *Server) startMemoryRound(env protocol.Envelope, conversationID string, messages []llm.Message) {
	s.Fire(s.cfg.Memory, "recall", protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: conversationID,
		Messages:       messages,
		Boss:           env.Boss,
	}}, env.Trace, s.cfg.Name)
}

// actionUserMessage 是链的入口：boss 发来用户消息。
// 半启动：把任务上下文托管给记忆服务（recall）——收到来自记忆的 resume 才完全启动。
func (s *Server) actionUserMessage(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.UserMessagePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Text) == "" && len(p.Images) == 0 {
		return nil, errors.New("user_message: text 与 images 至少必填其一")
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}

	// 记一条用户消息（尽力送达；幂等与去重在记忆服务侧处理）。
	s.Fire(s.cfg.Memory, "store", protocol.StorePayload{
		ConversationID: cid, Role: "user", Content: p.Text,
	}, env.Trace, s.cfg.Name)

	messages := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: p.Text, ImageIDs: p.Images},
	}
	s.startMemoryRound(env, cid, messages)
	return map[string]any{"status": "accepted"}, nil
}

// actionResume 是完全启动的唯一入口：仅接受来自记忆服务的 resume。
// 记忆组装好的上下文到达后，开启任务（黑板）并立即受理——推理在后台推进（见 board.go）。
func (s *Server) actionResume(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if env.From != s.cfg.Memory {
		return nil, fmt.Errorf("resume 仅接受来自记忆服务（%s）的调用，拒绝来源 %q", s.cfg.Memory, env.From)
	}
	var p protocol.ResumePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	tc := p.Context
	if len(tc.Messages) == 0 {
		return nil, errors.New("resume: 上下文为空")
	}
	// 该跳信封 boss 是 llm 自身（记忆为 llm 工作）；任务原 boss 随上下文恢复。
	boss := tc.Boss
	if boss == "" {
		boss = env.Boss
	}
	cid := tc.ConversationID
	if cid == "" {
		cid = "default"
	}
	st := s.boardOpen(cid, boss, env.Trace, tc.Messages)
	s.boardKick(st)
	return map[string]any{"status": "accepted"}, nil
}
