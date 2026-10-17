package tests

// M5 全链测试（异步工具 + 完全启动）：user_message → 记忆往返（recall → resume）→ 工具派发
// （即时受理回执）→ 自跳(step) → LLM 调 wait 阻塞至结果入账 → 收尾轮 → 交付(deliver) 直达 boss。

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

// stubBoss 是“老板”的最小替身：实现约定的 deliver 动作，收到结果放进 channel。
type stubBoss struct {
	ln        net.Listener
	srv       *http.Server
	delivered chan protocol.DeliverPayload
}

func startStubBoss(t *testing.T, center *stubCenter) *stubBoss {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	b := &stubBoss{ln: ln, delivered: make(chan protocol.DeliverPayload, 4)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", b.handleWS)
	b.srv = &http.Server{Handler: mux}
	go func() { _ = b.srv.Serve(ln) }()
	t.Cleanup(func() { _ = b.srv.Close() })

	// 把 boss 名片放进中心（reasoner 从 roster 解析它的地址）。
	center.mu.Lock()
	center.cards["stub-boss"] = protocol.ServiceCard{
		Name:        "stub-boss",
		Addr:        b.URL(),
		Description: "老板测试替身",
		Provides:    []protocol.Provide{{Action: "deliver", Input: "{text}", Output: "回执"}},
	}
	center.mu.Unlock()
	return b
}

func (b *stubBoss) URL() string { return "ws://" + b.ln.Addr().String() + "/ws" }

func (b *stubBoss) handleWS(w http.ResponseWriter, r *http.Request) {
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
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		if jp.Action == "deliver" {
			var p protocol.DeliverPayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			select {
			case b.delivered <- p:
			default:
			}
			resp, _ := protocol.NewResponse(env, "stub-boss", protocol.ResponsePayload{OK: true})
			c.send(resp)
		}
	}
}

// TestInferChain 验证 Fake LLM 驱动的全链（异步工具）：
// user_message（快速回执）→ LLM 决定调 stub-center__ping（已提供工具、真跳转、后台执行）
// → 自跳后 LLM 调 wait（阻塞到结果入账）→ LLM 收尾输出文本 → deliver 直达 boss。
func TestInferChain(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)

	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "stub-center__ping", Arguments: `{}`},
		}}},
		// 第二轮调 wait：阻塞到 ping 结果入账（让时序与断言可预期）。
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_wait", Type: "function",
			Function: llm.FunctionCall{Name: "wait", Arguments: `{}`},
		}}},
		{Role: "assistant", Content: "最终答复：你好！"},
	}}

	logger := log.New(os.Stdout, "[chain-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)

	// 发起链：boss=stub-boss——“结果交付对象”。
	raw, err := protocol.EncodePayload(map[string]any{"text": "你好"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "stub-boss"
	resp, err := c.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.ResponsePayload
	if err := resp.DecodePayload(&ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Fatalf("user_message 未被受理: %s", ack.Error)
	}

	// 等待 boss 收到 deliver（链在后台推进）。
	select {
	case d := <-boss.delivered:
		if d.Text != "最终答复：你好！" {
			t.Fatalf("交付内容不符: %q", d.Text)
		}
		if d.ConversationID != "default" {
			t.Fatalf("交付应带回会话标识: %q", d.ConversationID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	// 校验异步工具链：派发轮（回执）→ wait 轮（阻塞等待）→ 收尾轮——Fake 共收到 3 次请求。
	if fake.CallCount() != 3 {
		t.Fatalf("Fake LLM 调用次数 = %d, want 3", fake.CallCount())
	}
	first := fake.Requests[0]
	// 名册动作映射为工具：显式声明 tool:true 的跨服务动作在列表中；未声明的动作、
	// 调试动作（echo / debug_jump）不暴露；内置 wait 恒在。
	var hasPing, hasWait, hasEcho, hasDebug, hasHidden bool
	for _, tool := range first.Tools {
		switch tool.Function.Name {
		case "stub-center__ping":
			hasPing = true
		case "wait":
			hasWait = true
		case "reasoner__echo":
			hasEcho = true
		case "reasoner__debug_jump":
			hasDebug = true
		case "stub-center__hidden_no_tool":
			hasHidden = true
		}
	}
	if !hasPing {
		t.Fatalf("第一轮请求应包含 stub-center__ping 工具: %+v", first.Tools)
	}
	if !hasWait {
		t.Fatalf("工具列表应包含内置 wait: %+v", first.Tools)
	}
	if hasEcho || hasDebug || hasHidden {
		t.Fatalf("未声明 tool / 调试动作不应暴露为工具: %+v", first.Tools)
	}
	// 完全启动链：请求必须携带记忆块（由 resume 注入）。
	if !hasMemoryBlock(first.Messages) {
		t.Fatalf("第一轮请求应含记忆块: %+v", first.Messages)
	}
	// 第二轮请求：ping 已受理——回执（tool 消息）占住 tool_calls 的配对位。
	second := fake.Requests[1]
	hasAck := false
	for _, m := range second.Messages {
		if m.Role == "tool" && strings.Contains(m.Content, "已受理") && strings.Contains(m.Content, "call_1") {
			hasAck = true
		}
	}
	if !hasAck {
		t.Fatalf("第二轮请求应包含 ping 的受理回执: %+v", second.Messages)
	}
	// 第三轮请求（wait 返回后）：ping 结果已作为 [工具结果] 消息注入。
	third := fake.Requests[2]
	hasResult := false
	for _, m := range third.Messages {
		if strings.Contains(m.Content, "错误") {
			t.Fatalf("工具执行出错: %s", m.Content)
		}
		if m.Role == "user" && strings.Contains(m.Content, "[工具结果]") && strings.Contains(m.Content, "pong") {
			hasResult = true
		}
	}
	if !hasResult {
		t.Fatalf("第三轮请求应包含 ping 的 [工具结果] 消息: %+v", third.Messages)
	}
	if !hasMemoryBlock(third.Messages) {
		t.Fatalf("第三轮请求应含记忆块: %+v", third.Messages)
	}

	// 记忆写入：用户消息（链入口）、工具轨迹（命令 + 输出）与最终答复（交付后）都会记一条。
	stores := waitStoresUntil(t, mem, func(ss []protocol.StorePayload) bool {
		for _, s := range ss {
			if s.Content == "最终答复：你好！" {
				return true
			}
		}
		return false
	}, 5*time.Second)
	var gotUser, gotAssistant, gotCall, gotResult bool
	lastSeq := int64(0)
	for _, s := range stores {
		if s.Seq <= lastSeq {
			t.Fatalf("seq 必须单调递增: %+v", stores)
		}
		lastSeq = s.Seq
		switch {
		case s.Role == "user" && s.Kind == "user" && s.Content == "你好":
			gotUser = true
		case s.Role == "assistant" && s.Kind == "assistant" && s.Content == "最终答复：你好！":
			gotAssistant = true
		case s.Kind == "tool_call" && s.Content != "":
			gotCall = true
		case s.Kind == "tool_result" && s.Content != "":
			gotResult = true
		}
	}
	if !gotUser || !gotAssistant || !gotCall || !gotResult {
		t.Fatalf("store 内容不符（user=%v assistant=%v tool_call=%v tool_result=%v）: %+v",
			gotUser, gotAssistant, gotCall, gotResult, stores)
	}
}

// TestUnadvertisedToolRejected 验证执行侧闸门：模型（幻觉 / 注入）指名调用未提供或保留动作时，
// 执行被拒绝——错误作为 [工具结果] 回给模型，不会发生真实跳转；链本身继续推进。
func TestUnadvertisedToolRejected(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)

	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_bad", Type: "function",
			// 保留动作 + 借 debug_jump 伪造一次交付（“万能遥控”的典型用法）。
			Function: llm.FunctionCall{Name: "reasoner__debug_jump", Arguments: `{"to":"stub-boss","action":"deliver","input":{"text":"绕过"}}`},
		}}},
		// 第二轮调 wait：阻塞到上面那次调用（应被拒）入账。
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_wait", Type: "function",
			Function: llm.FunctionCall{Name: "wait", Arguments: `{}`},
		}}},
		{Role: "assistant", Content: "已被拒绝"},
	}}

	logger := log.New(os.Stdout, "[gate-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(map[string]any{"text": "你好"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "stub-boss"
	if _, err := c.Call(ctx, req); err != nil {
		t.Fatal(err)
	}

	// 链照常收尾：boss 收到的第一条交付是最终答复，而不是 debug_jump 若被执行会伪造的“绕过”。
	select {
	case d := <-boss.delivered:
		if d.Text != "已被拒绝" {
			t.Fatalf("交付内容不符（debug_jump 被真的执行了？）: %q", d.Text)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}
	select {
	case d := <-boss.delivered:
		t.Fatalf("出现预期外的交付（闸门失效？）: %+v", d)
	case <-time.After(500 * time.Millisecond):
	}

	if fake.CallCount() != 3 {
		t.Fatalf("Fake LLM 调用次数 = %d, want 3", fake.CallCount())
	}
	// 被拒的调用以“错误”形态进入 [工具结果]。
	third := fake.Requests[2]
	hasReject := false
	for _, m := range third.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "[工具结果]") &&
			strings.Contains(m.Content, "reasoner__debug_jump") && strings.Contains(m.Content, "不可用") {
			hasReject = true
		}
	}
	if !hasReject {
		t.Fatalf("第三轮请求应包含被拒的 [工具结果]: %+v", third.Messages)
	}
}

// hasMemoryBlock 判断消息链里是否有记忆块。
func hasMemoryBlock(messages []llm.Message) bool {
	for _, m := range messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, "【记忆】") {
			return true
		}
	}
	return false
}

// waitStores 等待记忆替身收到至少 n 条 store。
func waitStores(t *testing.T, m *stubMemory, n int, timeout time.Duration) []protocol.StorePayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := m.Stores(); len(s) >= n {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 %d 条 store 超时（实际 %d）", n, len(m.Stores()))
	return nil
}

// waitStoresUntil 等记忆替身收到满足条件的 store（记录数不再固定，按内容等更稳）。
func waitStoresUntil(t *testing.T, m *stubMemory, ok func([]protocol.StorePayload) bool, timeout time.Duration) []protocol.StorePayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if s := m.Stores(); ok(s) {
			return s
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 store 条件超时（实际 %+v）", m.Stores())
	return nil
}

// TestResumeRejected 验证“完全启动”入口只接受 from=memory——其他来源一律拒绝。
func TestResumeRejected(t *testing.T) {
	sc := startStubCenter(t)
	s := startReasoner(t, sc.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialReasoner(t, s)

	rp := jump(t, c, "resume", protocol.ResumePayload{Context: protocol.TaskContext{
		Messages: []llm.Message{{Role: "user", Content: "x"}},
	}}, ctx)
	if rp.OK {
		t.Fatal("resume 应拒绝非记忆来源的调用")
	}
	if !strings.Contains(rp.Error, "仅接受来自记忆服务") {
		t.Fatalf("错误信息不符: %s", rp.Error)
	}
}
