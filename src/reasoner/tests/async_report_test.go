// 异步受理的补报（report_result）测试：
//   - 受理即回执的动作（Provide.Async）→ 受理回执带回 item_id，句柄在 reasoner 侧登记；
//   - 补报到达时任务还在跑 → 注入同一条任务链（响应自证 delivery=injected）；
//   - 补报到达时任务已收尾 → 开补报轮，把结果补交给 boss（delivery=new_task，结果不丢）；
//   - 无对应受理的补报被忽略（不打扰任何任务）；缺 item_id 明确报错；
//   - wait 如实披露“已受理未回报”的异步任务（受理 ≠ 结果，不阻塞等待）。
package tests

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

// ---------- “受理即回执”的工具替身 ----------

// stubAsync 是异步工具替身：run 立刻回执 {item_id}；补报由测试自己按需发出（时序可控，不靠 sleep 猜）。
type stubAsync struct {
	ln     net.Listener
	srv    *http.Server
	itemID string
	kind   string

	got       chan protocol.ShellRunPayload // 收到的 run 输入
	envelope  chan protocol.Envelope
	beforeAck func(protocol.Envelope)
	boss      chan string // 受理时信封里的 boss
}

func startStubAsync(t *testing.T, center *stubCenter, itemID string, kinds ...string) *stubAsync {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &stubAsync{ln: ln, itemID: itemID, got: make(chan protocol.ShellRunPayload, 8), boss: make(chan string, 8), envelope: make(chan protocol.Envelope, 8)}
	if len(kinds) > 0 {
		a.kind = kinds[0]
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", a.handleWS)
	a.srv = &http.Server{Handler: mux}
	go func() { _ = a.srv.Serve(ln) }()
	t.Cleanup(func() { _ = a.srv.Close() })

	center.mu.Lock()
	center.cards["stub-async"] = protocol.ServiceCard{
		Name: "stub-async", Addr: a.URL(), Description: "异步工具测试替身",
		Provides: []protocol.Provide{{Action: "run", Tool: true, Async: true, Input: "{command}", Output: "受理回执"}},
	}
	center.mu.Unlock()
	return a
}

func (a *stubAsync) URL() string { return "ws://" + a.ln.Addr().String() + "/ws" }

func (a *stubAsync) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil || env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil || jp.Action != "run" {
			continue
		}
		var p protocol.ShellRunPayload
		_ = protocol.DecodeRaw(jp.Input, &p)
		select {
		case a.got <- p:
		default:
		}
		select {
		case a.boss <- env.Boss:
		default:
		}
		a.envelope <- env
		if a.beforeAck != nil {
			a.beforeAck(env)
		}
		// 受理即回执：只回句柄，不执行。
		out, _ := protocol.EncodePayload(map[string]any{"item_id": a.itemID, "phase": "queued", "kind": a.kind})
		resp, _ := protocol.NewResponse(env, "stub-async", protocol.ResponsePayload{OK: true, Data: out})
		c.send(resp)
	}
}

// ---------- 环境与辅助 ----------

// startReportEnv 起一套最小环境：中心 / boss / 记忆 / reasoner（真实例）。
func startReportEnv(t *testing.T, sc *stubCenter, fake *llm.Fake, setup ...func(*stubBoss, *stubMemory)) (*server.Server, *stubBoss, *stubMemory) {
	t.Helper()
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	for _, configure := range setup {
		configure(boss, mem)
	}
	logger := log.New(os.Stdout, "[report-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, boss, mem
}

// asyncOutcome 是补报响应的可观察结果。
type asyncOutcome struct {
	Status   string `msgpack:"status"`
	Delivery string `msgpack:"delivery"`
}

// fireReport 以“异步服务”的身份向 reasoner 送一条补报。
func fireReport(t *testing.T, s *server.Server, trace, item, phase, text string, final bool) asyncOutcome {
	t.Helper()
	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, _ := protocol.EncodePayload(protocol.AsyncResultPayload{ItemID: item, Phase: phase, Text: text, Final: final})
	env, _ := protocol.NewEnvelope(protocol.TypeJump, "stub-async", "reasoner", trace, protocol.JumpPayload{Action: "report_result", Input: raw})
	response, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := response.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("补报未被受理: %s", rp.Error)
	}
	var out asyncOutcome
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// reportUntilAccepted 重试送补报，直到 reasoner 认领。
// 登记发生在受理回执返回之后（工具调用在后台跑），此处用“响应自证”代替 sleep 猜时序。
func reportUntilAccepted(t *testing.T, s *server.Server, trace, item, phase, text string, final bool) asyncOutcome {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		out := fireReport(t, s, trace, item, phase, text, final)
		if out.Status == "accepted" {
			return out
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("补报 %s 始终未被认领", item)
	return asyncOutcome{}
}

// waitDelivery 等待 boss 收到一次交付并校验文本。
func waitDelivery(t *testing.T, boss *stubBoss, want string) {
	t.Helper()
	select {
	case d := <-boss.delivered:
		if d.Text != want {
			t.Fatalf("交付文本不符: %q, want %q", d.Text, want)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("等待交付 %q 超时", want)
	}
}

// requestHas 判断一次 LLM 请求的消息链里是否包含全部片段。
func requestHas(req llm.ChatRequest, subs ...string) bool {
	for _, sub := range subs {
		found := false
		for _, m := range req.Messages {
			if strings.Contains(m.Content, sub) {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ---------- 用例 ----------

// TestReportResultOpensSupplementAfterClose：模型在结果到达前收尾（受理即回执，不必等），
// 终态补报到达时任务已收尾 → 开补报轮把结果补交给 boss（独立任务链：system + 补报）。
func TestReportResultOpensSupplementAfterClose(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "cmd-1")
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo hi"}`},
		}}},
		// 受理回执在后台到账，用 wait 等它入账（不让“提前收尾被拒”干扰本用例的时序）。
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_wait", Type: "function",
			Function: llm.FunctionCall{Name: "wait", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "已受理，结果稍后送达"},
		{Role: "assistant", Content: "补充：hello"},
	}}
	s, boss, _ := startReportEnv(t, sc, fake)
	sendUser(t, s, "跑个命令")

	select {
	case got := <-async.got:
		if got.Command != "echo hi" {
			t.Fatalf("工具输入未透传: %+v", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("异步工具未被调用")
	}
	// boss 随工具调用传到受理方——受理方事后补报才知道结果面向谁。
	select {
	case b := <-async.boss:
		if b != "stub-boss" {
			t.Fatalf("工具调用未带上 boss: %q", b)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("未取到受理方看到的 boss")
	}

	// 第一交付：结果还没到，模型先说“已受理”（受理即回执，不该被长命令扣住）。
	waitDelivery(t, boss, "已受理，结果稍后送达")

	// 终态补报：任务已收尾 → 开补报轮。
	out := reportUntilAccepted(t, s, (<-async.envelope).Trace, "cmd-1", "done", "命令执行完成：hello", true)
	if out.Delivery != "new_task" {
		t.Fatalf("任务已收尾时应开补报轮: %+v", out)
	}
	waitDelivery(t, boss, "补充：hello")

	last := *fake.LastRequest()
	if len(last.Messages) != 2 {
		t.Fatalf("补报轮应是独立任务链（system + 补报），实际 %d 条: %+v", len(last.Messages), last.Messages)
	}
	if !requestHas(last, "[后续补报]", "stub-async__run", "命令执行完成：hello") {
		t.Fatalf("补报轮缺少补报内容: %+v", last.Messages)
	}
}

// TestReportResultNotSwallowedWhileDelivering：最终补报正好落在「最终答复正在交付、
// 任务还没收尾」的窗口里时，不能注入那个任务——它这一轮马上结束，boardKick 会因为
// running 不开新轮次，收尾后 gen 检查又认为没有未处理进展，注入的消息既不会被 LLM
// 看到，也不会再交付给 boss，等于静默丢掉。正确行为是提前按已收尾处理，走开补报轮。
func TestReportResultNotSwallowedWhileDelivering(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "cmd-slow-deliver")
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo hi"}`},
		}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_wait", Type: "function",
			Function: llm.FunctionCall{Name: "wait", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "已受理，结果稍后送达"},
		{Role: "assistant", Content: "补充：hello"},
	}}
	s, boss, _ := startReportEnv(t, sc, fake, func(b *stubBoss, _ *stubMemory) {
		b.setDeliverDelay(time.Second)
	})
	sendUser(t, s, "跑个命令")
	<-async.got

	// boss 收到第一交付 ⇒ reasoner 此刻正卡在等 boss 的响应，正是那个窗口。
	select {
	case d := <-boss.delivered:
		if d.Text != "已受理，结果稍后送达" {
			t.Fatalf("第一交付文本不符: %q", d.Text)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("等待第一交付超时")
	}

	out := reportUntilAccepted(t, s, (<-async.envelope).Trace, "cmd-slow-deliver", "done", "命令执行完成：hello", true)
	if out.Delivery != "new_task" {
		t.Fatalf("交付窗口内的补报应开补报轮（注入会被吞掉）: %+v", out)
	}
	boss.setDeliverDelay(0)
	waitDelivery(t, boss, "补充：hello")
}

// TestReportResultInjectedIntoLiveTask：补报到达时任务还在跑 → 注入同一条链，并唤醒新一轮；
// 该轮能看到补报 + 未完成的慢工具结果 + 原用户消息（证明是注入，不是另起任务）。
func TestReportResultInjectedIntoLiveTask(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "cmd-2")
	slow := startStubSlowSleep(t, sc, 400*time.Millisecond)
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo hi"}`},
		}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_2", Type: "function",
			Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "（提前收尾：应被拒绝）"},
		{Role: "assistant", Content: "最终交付：hello"},
	}}
	s, boss, _ := startReportEnv(t, sc, fake)
	sendUser(t, s, "跑个命令")
	<-async.got

	// 等慢工具真的开始跑：此刻任务一定还在（既没收尾也没被取代）。
	deadline := time.Now().Add(15 * time.Second)
	for {
		if calls, _ := slow.Stats(); calls > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("慢工具迟迟未被派发")
		}
		time.Sleep(10 * time.Millisecond)
	}

	out := reportUntilAccepted(t, s, (<-async.envelope).Trace, "cmd-2", "approved", "审批通过，已开始执行", false)
	if out.Delivery != "injected" {
		t.Fatalf("任务在跑时应注入同一条链: %+v", out)
	}

	waitDelivery(t, boss, "最终交付：hello")
	last := *fake.LastRequest()
	if !requestHas(last, "[后续补报]", "审批通过，已开始执行", "[工具结果] slow__work", "跑个命令") {
		t.Fatalf("收尾轮应同时看到补报、慢工具结果与原用户消息: %+v", last.Messages)
	}
}

// TestWaitDisclosesOutstandingAsync：wait 不阻塞等待异步补报，但如实披露“已受理未回报”。
func TestWaitDisclosesOutstandingAsync(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "cmd-3")
	startStubSlowSleep(t, sc, 150*time.Millisecond)
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo hi"}`},
		}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_2", Type: "function",
			Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
		}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_wait", Type: "function",
			Function: llm.FunctionCall{Name: "wait", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "结果稍后送达"},
	}}
	s, boss, _ := startReportEnv(t, sc, fake)
	sendUser(t, s, "跑个命令")
	<-async.got

	waitDelivery(t, boss, "结果稍后送达")
	last := *fake.LastRequest()
	if !requestHas(last, "已受理的异步任务尚未回报", "cmd-3") {
		t.Fatalf("wait 应披露未回报的异步任务: %+v", last.Messages)
	}
	// 披露不是阻塞：任务在异步受理仍未回报时就已经收尾（上面的交付即证据）。
}

// TestReportWithoutMatchingItemIgnored：句柄对不上的补报被忽略（不开启任务、不打扰模型、不交付）。
func TestReportWithoutMatchingItemIgnored(t *testing.T) {
	sc := startStubCenter(t)
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "hi"}}}
	s, boss, _ := startReportEnv(t, sc, fake)

	out := fireReport(t, s, "unknown-trace", "no-such-item", "done", "x", true)
	if out.Status != "ignored" {
		t.Fatalf("无对应受理的补报应被忽略: %+v", out)
	}
	if n := fake.CallCount(); n != 0 {
		t.Fatalf("忽略的补报不应触发 LLM 调用: %d", n)
	}
	select {
	case d := <-boss.delivered:
		t.Fatalf("忽略的补报不应有交付: %+v", d)
	case <-time.After(300 * time.Millisecond):
	}

	// 缺 item_id：明确报错，不静默。
	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if rp := jump(t, c, "report_result", protocol.AsyncResultPayload{Phase: "done"}, ctx); rp.OK {
		t.Fatal("缺 item_id 的补报应被拒")
	}
}

// TestReportBeforeAcceptanceResponse 使用真实 WS；工具必须先收到补报回执，才发送工具受理回执。
// 因而时序由协议屏障决定，不依赖 sleep 或机器速度。
func TestReportBeforeAcceptanceResponse(t *testing.T) {
	sc := startStubCenter(t)
	async := startStubAsync(t, sc, "early-item")
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "early-call", Type: "function", Function: llm.FunctionCall{Name: "stub-async__run", Arguments: `{"command":"echo early"}`}}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "wait-call", Type: "function", Function: llm.FunctionCall{Name: "wait", Arguments: "{}"}}}},
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("judge", `{"status":"done"}`)}},
		{Role: "assistant", Content: "已收到提前完成的结果"},
	}}
	s, boss, mem := startReportEnv(t, sc, fake, func(b *stubBoss, m *stubMemory) {
		m.setTask(&protocol.Task{ID: "original-task", Name: "早到回报", Status: "active"})
		b.progress = make(chan protocol.ProgressPayload, 64)
		sc.mu.Lock()
		card := sc.cards["stub-boss"]
		card.Provides = append(card.Provides, protocol.Provide{Action: "progress"})
		sc.cards["stub-boss"] = card
		sc.mu.Unlock()
	})
	result := make(chan string, 1)
	async.beforeAck = func(call protocol.Envelope) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := protocol.DialContext(ctx, s.WsURL(), nil)
		if err != nil {
			result <- err.Error()
			return
		}
		defer c.Close()
		input, _ := protocol.EncodePayload(protocol.AsyncResultPayload{ItemID: "early-item", Phase: "done", Text: "实际早到的输出", Final: true})
		env, _ := protocol.NewEnvelope(protocol.TypeJump, "stub-async", "reasoner", call.Trace, protocol.JumpPayload{Action: "report_result", Input: input})
		reply, err := c.Call(ctx, env)
		if err != nil {
			result <- err.Error()
			return
		}
		var rp protocol.ResponsePayload
		if err := reply.DecodePayload(&rp); err != nil {
			result <- err.Error()
			return
		}
		if !rp.OK {
			result <- rp.Error
			return
		}
		var out asyncOutcome
		if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
			result <- err.Error()
			return
		}
		mem.setTask(&protocol.Task{ID: "newer-task", Name: "新任务", Status: "active"})
		result <- out.Status
	}
	sendUser(t, s, "测试早到回报")
	select {
	case status := <-result:
		if status != "buffered" {
			t.Fatalf("回执发送之前应已缓存，got %q", status)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("工具未完成补报屏障")
	}
	waitDelivery(t, boss, "已收到提前完成的结果")
	if !requestHas(*fake.LastRequest(), "实际早到的输出", "job=early-call", "item=early-item") {
		t.Fatalf("真实 Reasoner 未收到匹配的早到结果: %+v", fake.LastRequest().Messages)
	}
	// 记忆服务当前任务已经改变，历史输出仍必须带上原 task_id，不能依赖服务端猜归属。
	deadline := time.Now().Add(5 * time.Second)
	foundStore := false
	for time.Now().Before(deadline) && !foundStore {
		for _, stored := range mem.Stores() {
			if stored.Kind == "tool_result" && stored.Content == "实际早到的输出" {
				if stored.TaskID != "original-task" {
					t.Fatalf("store 归错任务: %+v", stored)
				}
				foundStore = true
			}
		}
		if !foundStore {
			time.Sleep(10 * time.Millisecond)
		}
	}
	if !foundStore {
		t.Fatal("没有收到补报输出的 store")
	}
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case progress := <-boss.progress:
			if progress.Kind == "tool_result" && progress.Content == "实际早到的输出" {
				if progress.TaskID != "original-task" {
					t.Fatalf("progress 归错任务: %+v", progress)
				}
				return
			}
		case <-timeout.C:
			t.Fatal("没有收到补报输出的 progress")
		}
	}
}
