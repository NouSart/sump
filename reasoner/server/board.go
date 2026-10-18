package server

// 黑板与轮次（异步工具模型的核心）：
//   - 任务窗口内，每个会话在进程内保留“最新消息链 + 未完成工具表”（唯一副本）；
//   - 工具调用 = 派发 + 即时受理回执（占住 API 的 tool_calls 配对位），执行在后台；
//   - 结果以 [工具结果] 消息注入黑板，并按通知模式唤醒下一轮；
//   - 收尾门禁：仍有工具在跑、或模型看过的快照已过期 → 拒绝收尾（暂存它的答复，等下一轮）；
//   - 窗口 = 任务在跑期：交付 / 被取代即释放（不落盘、不跨重启——见 SPEC §6）。
//
// 轮次衔接仍走自跳 step（发出即完；上下文在黑板里，信纸只带会话号）。
//
// 状态机（taskState 四态 + 水位；除 closed 的读取外均由本文件维护，st.mu 保护）：
//   - gen     消息链变化水位：派发回执 / 结果入账 / 拒绝收尾各自 +1；
//   - seen    已交给某轮 LLM 的水位：轮开始时对齐 gen；gen != seen ⇒ 存在“未处理的进展”，必须最终被一轮消化；
//   - running 每会话同时至多一轮 LLM（boardKick 互斥，防重入）；
//   - closed  已交付（boardClose）/ 被取代（boardOpen）：此后一切写入与唤醒直接丢弃。
//
// 唤醒契约（保证 gen != seen 终被消化；开轮统一经 boardKick）：
//   - 结果入账：each 模式每条都踢；batch 模式把 seen 推进到 gen（暂不踢），最后一条才踢；
//   - 拒绝收尾：仍有工具在跑时 seen = gen（“停车”），等结果入账来踢——避免空转烧 LLM；
//   - 轮末兜底：一轮结束时若 gen != seen（期间有新进展），自跳 step 补一轮。

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

const (
	waitToolName = "wait"                 // 内置工具名（不来自名册）
	waitCap      = 5 * time.Minute        // wait 的阻塞上限（安全阀；工具超时另计）
	waitPoll     = 100 * time.Millisecond // wait 轮询间隔
)

// pendingJob 是一个已受理、正在执行的工具调用。
type pendingJob struct {
	name string
	// async：这个工具是异步受理的（回执只是「已排队」，真正的输出靠后续补报）。
	// 这类工具的回执不记进轨迹——它没有信息量，真输出到达时再记一条。
	async bool
}

// taskState 是黑板里的一个会话任务（窗口 = 任务在跑期）。
type taskState struct {
	mu       sync.Mutex
	cid      string
	boss     string
	trace    string
	messages []llm.Message         // 最新完整历史（唯一副本；任务内各轮都基于它）
	pending  map[string]pendingJob // call_id → 正在执行的工具
	gen      int64                 // 消息链变化水位（每次变化 +1）
	seen     int64                 // 已安排给“下一轮”的水位（gen != seen = 有未处理的进展）
	running  bool                  // 是否有一轮 LLM 在跑（每会话同时至多一轮）
	closed   bool                  // 已交付 / 被取代：残留事件一律丢弃

	taskID      string // 这次工作所属的任务（记忆服务在 recall 时确定）
	taskName    string // 任务名（写进系统提示，也是历史列表的标题）
	judged      bool   // 本轮是否已用 task_status 判定过完成与否
	judgedDone  bool   // 判定结果：true = 任务完成
	judgedTitle string // 判定时顺带修正的任务名（空 = 不改）
	judgeAsks   int    // 已经为"必须先判定"催过几次（上限 2，防死循环）
}

// ---------- 黑板操作 ----------

// boardOpen 开启任务；同一会话已有进行中任务时，后者取代前者（旧任务的结果将被丢弃）。
func (s *Server) boardOpen(cid, boss, trace, taskID, taskName string, messages []llm.Message) *taskState {
	st := &taskState{
		cid: cid, boss: boss, trace: trace,
		taskID: taskID, taskName: taskName,
		messages: messages, pending: map[string]pendingJob{}, gen: 1,
	}
	s.boardMu.Lock()
	old := s.board[cid]
	s.board[cid] = st
	s.boardMu.Unlock()
	if old != nil {
		old.mu.Lock()
		old.closed = true
		old.mu.Unlock()
		s.logger.Printf("会话 %s：新任务取代进行中的旧任务（旧任务的在跑结果将被丢弃）", cid)
	}
	s.logger.Printf("会话 %s：任务开启（%d 条消息，boss=%q）", cid, len(messages), boss)
	return st
}

// boardGet 取会话的进行中任务（无则 nil）。
func (s *Server) boardGet(cid string) *taskState {
	s.boardMu.Lock()
	defer s.boardMu.Unlock()
	return s.board[cid]
}

// boardClose 交付完成：关闭任务并从黑板移除。
func (s *Server) boardClose(st *taskState) {
	st.mu.Lock()
	st.closed = true
	st.mu.Unlock()
	s.boardMu.Lock()
	if s.board[st.cid] == st {
		delete(s.board, st.cid)
	}
	s.boardMu.Unlock()
}

// boardKick 若有未处理的进展且当前没有轮在跑，开一轮（防轮次重入、防丢唤醒）。
func (s *Server) boardKick(st *taskState) {
	st.mu.Lock()
	if st.closed || st.running || st.gen == st.seen {
		st.mu.Unlock()
		return
	}
	st.running = true
	st.mu.Unlock()
	go func() {
		s.boardRound(st)
		st.mu.Lock()
		more := !st.closed && st.gen != st.seen
		st.running = false
		st.mu.Unlock()
		if more {
			s.fireStep(st)
		}
	}()
}

// fireStep 自跳推进下一轮（发出即完；信纸只带会话号）。
func (s *Server) fireStep(st *taskState) {
	st.mu.Lock()
	cid, trace, boss, closed := st.cid, st.trace, st.boss, st.closed
	st.mu.Unlock()
	if closed {
		return
	}
	s.Fire("self", "step", stepPayload{ConversationID: cid}, trace, boss)
}

// ---------- 轮次 ----------

// boardRound 跑一轮：LLM（用黑板快照）→ 决策（派发 / 收尾 / 拒绝收尾）。
func (s *Server) boardRound(st *taskState) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	snapshot := make([]llm.Message, len(st.messages))
	copy(snapshot, st.messages)
	st.seen = st.gen
	gen0 := st.gen
	st.mu.Unlock()

	client := s.llmClient()
	if client == nil {
		s.logger.Printf("会话 %s：LLM 未配置，任务停住（等下一次事件）", st.cid)
		return
	}
	resp, err := client.Chat(context.Background(), llm.ChatRequest{
		Messages: snapshot,
		Tools:    s.rosterTools(),
		Images:   s.resolveImages(context.Background(), st.trace, snapshot),
	})
	if err != nil {
		s.logger.Printf("会话 %s：LLM 调用失败: %v（任务停住，等下一次事件重试）", st.cid, err)
		return
	}

	// 思考先落库再派发：seq 保证「想了什么 → 跑了什么」的先后（给人看的历史）。
	s.record(st.cid, st.boss, st.trace, st.taskID, kindReasoning, "assistant", resp.Reasoning, nil)

	switch {
	case len(resp.ToolCalls) > 0:
		s.dispatchToolCalls(st, resp)
	case strings.TrimSpace(resp.Content) != "":
		s.tryClose(st, gen0, resp.Content)
	default:
		s.logger.Printf("会话 %s：LLM 返回为空（既无 tool_calls 也无内容）", st.cid)
	}
}

// dispatchToolCalls 处理一轮里的工具调用：助手消息与受理回执即刻入账（占住配对位），
// 执行转入后台；wait / task_status 是内置工具，不跳任何服务。
func (s *Server) dispatchToolCalls(st *taskState, resp llm.Message) {
	// task_status 先算：它要读改写黑板状态（自己会加锁），不能放在持锁段里做（不可重入）。
	judgements := make([]llm.Message, 0, 1)
	for _, call := range resp.ToolCalls {
		if call.Function.Name != taskStatusToolName {
			continue
		}
		judgements = append(judgements, llm.Message{Role: "tool", ToolCallID: call.ID, Content: s.applyTaskStatus(st, call)})
	}

	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	st.messages = append(st.messages, llm.Message{Role: "assistant", Content: resp.Content, ToolCalls: resp.ToolCalls})
	st.gen++
	// 又要派工具 = 又有了新进展：之前的判定作废，交付前必须重新判定。
	for _, call := range resp.ToolCalls {
		if call.Function.Name != waitToolName && call.Function.Name != taskStatusToolName {
			st.judged = false
			st.judgedDone = false
			break
		}
	}
	acks := make([]llm.Message, 0, len(resp.ToolCalls))
	dispatched := 0
	for _, call := range resp.ToolCalls {
		if call.Function.Name == waitToolName || call.Function.Name == taskStatusToolName {
			continue
		}
		st.pending[call.ID] = pendingJob{name: call.Function.Name}
		acks = append(acks, llm.Message{Role: "tool", ToolCallID: call.ID, Content: ackText(call)})
		dispatched++
	}
	st.messages = append(st.messages, acks...)
	st.messages = append(st.messages, judgements...)
	st.gen += int64(len(acks) + len(judgements))
	cid, boss, trace, taskID := st.cid, st.boss, st.trace, st.taskID
	st.mu.Unlock()

	// 命令先落库再执行：即使执行超时/进程崩了，"跑过什么"也该留在记忆里。
	for _, call := range resp.ToolCalls {
		if call.Function.Name == waitToolName || call.Function.Name == taskStatusToolName {
			continue
		}
		text, meta := toolCallTrace(call)
		s.record(cid, boss, trace, taskID, kindToolCall, "assistant", text, meta)
	}

	for _, call := range resp.ToolCalls {
		if call.Function.Name == waitToolName || call.Function.Name == taskStatusToolName {
			continue
		}
		go s.runToolJob(st, call)
	}
	if dispatched > 0 {
		s.logger.Printf("会话 %s：派发 %d 个工具（单会话并发上限 %d）", st.cid, dispatched, s.toolConcurrency())
	}

	var waits []llm.Message
	for _, call := range resp.ToolCalls {
		if call.Function.Name != waitToolName {
			continue
		}
		waits = append(waits, llm.Message{Role: "tool", ToolCallID: call.ID, Content: s.waitPending(st)})
	}
	if len(waits) > 0 {
		st.mu.Lock()
		if !st.closed {
			st.messages = append(st.messages, waits...)
			st.gen += int64(len(waits))
		}
		st.mu.Unlock()
	}
}

// waitPending 阻塞等待本任务的工具全部结束（上限 waitCap）。
// 异步受理（Provide.Async）不在等待之列——受理已完成，但它尚未回报：如实披露，不假装“都完成了”。
func (s *Server) waitPending(st *taskState) string {
	deadline := time.Now().Add(waitCap)
	for {
		st.mu.Lock()
		if st.closed {
			st.mu.Unlock()
			return "任务已被取代或结束"
		}
		n := len(st.pending)
		names := pendingNames(st.pending)
		st.mu.Unlock()
		if n == 0 {
			text := "全部工具任务已完成；结果已作为 [工具结果] 消息写入对话。"
			if async := s.outstandingAsync(st.cid); len(async) > 0 {
				text += fmt.Sprintf("另有 %d 个已受理的异步任务尚未回报（%s）：受理只表示已排队，其结果的到达方式是以 [后续补报] 开头的消息——在那之前不要断言执行结果。",
					len(async), strings.Join(async, ", "))
			}
			return text
		}
		if time.Now().After(deadline) {
			return fmt.Sprintf("等待超时：仍有 %d 个工具任务在运行（%s）；结果到达后会再次唤醒你。", n, names)
		}
		time.Sleep(waitPoll)
	}
}

// runToolJob 后台执行一次工具调用：会话级并发上限 + 单次工具超时；完成后结果入账。
func (s *Server) runToolJob(st *taskState, call llm.ToolCall) {
	ctx, cancel := context.WithTimeout(context.Background(), s.toolTimeout())
	defer cancel()

	if sem := s.toolSem(st.cid); sem != nil {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			s.boardResult(st, call, "错误: 等待并发额度超时")
			return
		}
	}
	content, err := s.runToolCall(ctx, st, call)
	if err != nil {
		s.logger.Printf("工具 %s 失败: %v", call.Function.Name, err)
		content = "错误: " + err.Error()
	} else {
		s.logger.Printf("工具 %s 完成: %.120s", call.Function.Name, content)
	}
	s.boardResult(st, call, content)
}

// boardResult 结果入账：清 pending、追加 [工具结果] 消息，并按通知模式决定是否唤醒。
// batch 模式在还有工具在跑时只入账不唤醒（唤醒攒到最后一个结果）。
func (s *Server) boardResult(st *taskState, call llm.ToolCall, content string) {
	mode := s.notifyMode()
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		s.logger.Printf("工具 %s 的结果到达，但任务已结束（丢弃）", call.Function.Name)
		return
	}
	if _, ok := st.pending[call.ID]; !ok {
		st.mu.Unlock()
		s.logger.Printf("工具 %s 的结果不属于当前任务（丢弃）", call.Function.Name)
		return
	}
	job := st.pending[call.ID]
	delete(st.pending, call.ID)
	st.messages = append(st.messages, llm.Message{Role: "user", Content: resultText(call, content)})
	st.gen++
	remaining := len(st.pending)
	if mode == "batch" && remaining > 0 {
		st.seen = st.gen // 攒批：非最后一条不唤醒
	}
	cid, boss, trace, taskID := st.cid, st.boss, st.trace, st.taskID
	st.mu.Unlock()
	if !job.async {
		s.record(cid, boss, trace, taskID, kindToolResult, "tool", content, map[string]any{"tool": call.Function.Name, "call_id": call.ID})
	}
	s.logger.Printf("会话 %s：工具 %s 结果入账（还剩 %d 个在跑）", st.cid, call.Function.Name, remaining)
	s.boardKick(st)
}

// tryClose 收尾判定：仍有工具在跑、或本轮的快照已过期（期间有新消息）→ 拒绝收尾。
func (s *Server) tryClose(st *taskState, gen0 int64, text string) {
	st.mu.Lock()
	if st.closed {
		st.mu.Unlock()
		return
	}
	pending := len(st.pending)
	names := pendingNames(st.pending)
	stale := st.gen != gen0
	if pending > 0 || stale {
		st.messages = append(st.messages,
			llm.Message{Role: "assistant", Content: text},
			llm.Message{Role: "user", Content: refuseNotice(pending, names)},
		)
		st.gen += 2
		if pending > 0 {
			st.seen = st.gen // 停车：结果入账时再唤醒（避免反复烧 LLM 轮次）
		}
		st.mu.Unlock()
		s.logger.Printf("会话 %s：拒绝收尾（在跑 %d 个工具，快照过期=%v）", st.cid, pending, stale)
		return
	}

	// 收尾门禁：出结果必须先判定任务完成情况（task_status）。
	// 没判定就催一轮（最多催两次）——两次都不判按"未完成"记账，照常交付（不制造死循环）。
	// 只有"有任务"时才设门禁：任务由记忆服务在 recall 时保证，拿不到任务就没有判定对象。
	if st.taskID != "" && !st.judged {
		if st.judgeAsks < 2 {
			st.judgeAsks++
			st.messages = append(st.messages,
				llm.Message{Role: "assistant", Content: text},
				llm.Message{Role: "user", Content: taskJudgeReminder},
			)
			st.gen += 2
			st.mu.Unlock()
			s.logger.Printf("会话 %s：拒绝收尾（还没判定任务是否完成，第 %d 次提醒）", st.cid, st.judgeAsks)
			return
		}
		s.logger.Printf("会话 %s：两次催促仍未判定任务，按未完成处理并交付", st.cid)
	}
	st.mu.Unlock()

	// 交付前复查：放行到真正交付之间，任务可能已被同会话新消息取代（boardOpen 置 closed）——
	// 取代后旧答复不再交付。
	st.mu.Lock()
	replaced := st.closed
	st.mu.Unlock()
	if replaced {
		s.logger.Printf("会话 %s：任务已被新消息取代，答复不再交付", st.cid)
		return
	}

	if err := s.deliverToBoss(st, text); err != nil {
		s.logger.Printf("会话 %s：交付 boss(%s) 失败: %v（按“送达后不重试”原则停住，等下一次事件）", st.cid, st.boss, err)
		return
	}

	// 交付在途被取代：答复已送达（无法撤回），仍写入记忆保持历史一致；“任务完成”日志让位。
	st.mu.Lock()
	replaced = st.closed
	st.mu.Unlock()

	s.Fire(s.memoryName(), "store", protocol.StorePayload{
		ConversationID: st.cid, Boss: st.boss, Role: "assistant", Content: text, Kind: "assistant", Seq: s.nextSeq(), TaskID: st.taskID,
	}, st.trace, s.cfg.Name)
	s.boardClose(st)
	if replaced {
		s.logger.Printf("会话 %s：交付完成（在途已被新任务取代；答复已送达并记入记忆）", st.cid)
	} else {
		s.logger.Printf("会话 %s：任务完成，已交付 boss(%s)", st.cid, st.boss)
	}
	// 复盘在交付之后、在后台：老板已经拿到答复，这里成败都不影响他（见 reflect.go）。
	// 被取代的任务不记——“这次干到一半被打断”不是可复用的经验，是噪音。
	if !replaced {
		s.reflectOnTask(st, text)
	}
}

// deliverToBoss 把最终结果直达 boss（短响应；Call 内部只对“拨号失败”安全重试）。
// 送不出去时给**入口方**补一条失败信息——boss 认错了/丢了的时候，那条是唯一的回声。
func (s *Server) deliverToBoss(st *taskState, text string) error {
	if st.boss == "" {
		return s.notifyDeliveryFailure(st, errors.New("无 boss，结果无法交付"))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := s.Call(ctx, st.boss, "deliver", protocol.DeliverPayload{Text: text, ConversationID: st.cid, TaskID: st.taskID}, st.trace, ""); err != nil {
		return s.notifyDeliveryFailure(st, fmt.Errorf("交付 boss(%s) 失败: %w", st.boss, err))
	}
	return nil
}

// noteEntry 记下会话的入口服务（user_message 的 from）——交付失败时的另一条回家路。
func (s *Server) noteEntry(cid, from string) {
	if cid == "" || from == "" {
		return
	}
	s.entryMu.Lock()
	s.entries[cid] = from
	s.entryMu.Unlock()
}

// entryOf 返回会话的入口服务名（没记过返回空）。
func (s *Server) entryOf(cid string) string {
	s.entryMu.Lock()
	defer s.entryMu.Unlock()
	return s.entries[cid]
}

// notifyDeliveryFailure 交付失败时按“最后失败了再补一条给 A”的规矩通知入口方：
// 入口就是 boss 时没必要重试同一地址（刚报过），但 boss 认错/丢了时这条是唯一能让用户
// 看见“任务失败了”的信息。它不改交付结果——失败仍是失败（返回 cause 供上层记日志）。
func (s *Server) notifyDeliveryFailure(st *taskState, cause error) error {
	entry := s.entryOf(st.cid)
	if entry == "" || entry == st.boss {
		return cause
	}
	text := fmt.Sprintf("[交付失败] 上一轮任务的结果没能送回来：%v", cause)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := s.Call(ctx, entry, "deliver", protocol.DeliverPayload{Text: text, ConversationID: st.cid}, st.trace, ""); err != nil {
		s.logger.Printf("会话 %s：失败补报也送不出去（入口 %s）: %v", st.cid, entry, err)
		return cause
	}
	s.logger.Printf("会话 %s：交付失败已补报给入口 %s", st.cid, entry)
	return cause
}

// ---------- 自跳入口 ----------

// stepPayload 是自跳（step）的 payload：只带会话号（上下文在黑板里）。
type stepPayload struct {
	ConversationID string `msgpack:"conversation_id,omitempty"`
}

// actionStep 承接自跳：把黑板上的任务推进到下一轮。
func (s *Server) actionStep(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p stepPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	st := s.boardGet(cid)
	if st == nil {
		s.logger.Printf("step：会话 %s 没有进行中的任务（黑板已释放，忽略）", cid)
		return map[string]any{"status": "ignored"}, nil
	}
	s.boardKick(st)
	return map[string]any{"status": "accepted"}, nil
}

// ---------- 文案与辅助 ----------

// ackText 是受理回执（tool 消息）：占住该 tool_call 的配对位。
func ackText(call llm.ToolCall) string {
	return fmt.Sprintf("已受理：%s 正在执行（job=%s）；结果稍后作为 [工具结果] 消息送达（可用 %s 工具等待）。",
		call.Function.Name, call.ID, waitToolName)
}

// resultText 是注入黑板的工具结果（role=user：外部世界的回报，不是 assistant 发言）。
func resultText(call llm.ToolCall, content string) string {
	return fmt.Sprintf("[工具结果] %s（job=%s）：\n%s", call.Function.Name, call.ID, content)
}

// refuseNotice 是拒绝收尾时注入的提示。
func refuseNotice(pending int, names string) string {
	if pending > 0 {
		return fmt.Sprintf("【系统】尚有 %d 个工具任务在运行（%s）。现在不能收尾；结果到齐后你会被再次唤醒（也可调用 %s 立即等待）。",
			pending, names, waitToolName)
	}
	return "【系统】你的上下文已过期（等待期间有新消息到达）。请基于最新消息重新给出最终答复。"
}

// pendingNames 格式化在跑的工具（名 + job；排序保证可读性）。
func pendingNames(pending map[string]pendingJob) string {
	names := make([]string, 0, len(pending))
	for id, job := range pending {
		names = append(names, job.name+"(job="+id+")")
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}
