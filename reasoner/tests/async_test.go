package tests

// 异步工具模型测试：受理回执 / 结果注入 / 收尾门禁（拒绝提前收尾）/ 攒批唤醒（batch）。
// 用 stubSlow（每次 work 睡 80ms）制造“结果晚于推理轮到达”的确定性时序。

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

// startAsyncEnv 起一套最小环境：中心 / boss / 记忆 / 慢工具 / reasoner。
func startAsyncEnv(t *testing.T, sc *stubCenter, fake *llm.Fake, notify string) (*server.Server, *stubBoss) {
	t.Helper()
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	startStubSlow(t, sc)

	logger := log.New(os.Stdout, "[async-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		NotifyMode:        notify,
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, boss
}

// sendUser 向 reasoner 发一条 user_message（boss=stub-boss）并断言被受理。
func sendUser(t *testing.T, s *server.Server, text string) {
	t.Helper()
	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(map[string]any{"text": text})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "stub-boss"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
}

// hasToolResult 判断一轮请求里是否注入了某个 job 的 [工具结果]。
func hasToolResult(req llm.ChatRequest, jobID string) bool {
	for _, m := range req.Messages {
		if m.Role == "user" && strings.Contains(m.Content, "[工具结果]") && strings.Contains(m.Content, "job="+jobID) {
			return true
		}
	}
	return false
}

// TestAsyncRefuseCloseThenDeliver：结果未到时模型提前收尾 → 被拒绝（暂存）；
// 结果入账唤醒新一轮后才真正交付；只交付一次。
func TestAsyncRefuseCloseThenDeliver(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_1", Type: "function",
			Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"},
		}}},
		{Role: "assistant", Content: "结果已就绪"}, // 慢工具（80ms）尚未完成时就会返回
	}}
	s, boss := startAsyncEnv(t, startStubCenter(t), fake, "")
	sendUser(t, s, "跑一个慢任务")

	select {
	case d := <-boss.delivered:
		if d.Text != "结果已就绪" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}
	// 只交付一次（提前的答复被拒绝、未泄漏成两次交付）。
	select {
	case d := <-boss.delivered:
		t.Fatalf("重复交付: %+v", d)
	case <-time.After(300 * time.Millisecond):
	}

	// 派发轮 → 提前收尾被拒轮 → 结果入账后的收尾轮。
	if fake.CallCount() != 3 {
		t.Fatalf("Fake LLM 调用次数 = %d, want 3", fake.CallCount())
	}
	last := *fake.LastRequest()
	hasNotice, hasResult, hasAck := false, false, false
	for _, m := range last.Messages {
		if strings.Contains(m.Content, "【系统】") {
			hasNotice = true
		}
		if m.Role == "user" && strings.Contains(m.Content, "[工具结果]") && strings.Contains(m.Content, "slow__work") {
			hasResult = true
		}
		if m.Role == "tool" && strings.Contains(m.Content, "已受理") {
			hasAck = true
		}
	}
	if !hasAck {
		t.Fatalf("最后一轮请求应含受理回执: %+v", last.Messages)
	}
	if !hasNotice {
		t.Fatalf("最后一轮请求应含【系统】拒绝收尾提示: %+v", last.Messages)
	}
	if !hasResult {
		t.Fatalf("最后一轮请求应含 [工具结果]: %+v", last.Messages)
	}
}

// TestNotifyModeBatch：batch 模式下多个结果只唤醒一次——任何一轮请求里，
// 两条结果要么都在、要么都不在（不会单独出现某一条）。
func TestNotifyModeBatch(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_0", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
			{ID: "call_1", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
		}},
		{Role: "assistant", Content: "两个都完成了"},
	}}
	s, boss := startAsyncEnv(t, startStubCenter(t), fake, "batch")
	sendUser(t, s, "跑两个慢任务")

	select {
	case d := <-boss.delivered:
		if d.Text != "两个都完成了" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if fake.CallCount() < 2 {
		t.Fatalf("Fake LLM 调用次数 = %d, want ≥2", fake.CallCount())
	}
	for i, req := range fake.Requests {
		if hasToolResult(req, "call_0") != hasToolResult(req, "call_1") {
			t.Fatalf("batch 模式下不应只出现一个结果（第 %d 轮请求）: %+v", i+1, req.Messages)
		}
	}
	// 最终一轮两条结果都在。
	if !hasToolResult(*fake.LastRequest(), "call_0") || !hasToolResult(*fake.LastRequest(), "call_1") {
		t.Fatalf("最后一轮请求应含两条 [工具结果]: %+v", fake.LastRequest().Messages)
	}
}

// TestConfigureLive：设置中心下发 configure（tool.notify_mode=batch）立即生效——
// 不需要重启，后续工具结果按攒批唤醒（结果成对出现，不会单独唤醒某一条）。
func TestConfigureLive(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_0", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
			{ID: "call_1", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
		}},
		{Role: "assistant", Content: "configure 生效"},
	}}
	s, boss := startAsyncEnv(t, startStubCenter(t), fake, "each")

	// 模拟设置中心推送：tool.notify_mode = batch。
	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rp := jump(t, c, "configure", protocol.ConfigurePayload{Key: "tool.notify_mode", Value: "batch", Default: "each", Overridden: true}, ctx)
	if !rp.OK {
		t.Fatalf("configure 未被受理: %s", rp.Error)
	}
	// 未支持的设置键：明确拒绝（回执不“假装已应用”）。
	bad := jump(t, c, "configure", protocol.ConfigurePayload{Key: "no.such.key", Value: "x"}, ctx)
	if bad.OK {
		t.Fatal("未支持的设置键应被拒绝")
	}
	if !strings.Contains(bad.Error, "未支持的设置键") {
		t.Fatalf("拒绝理由不符: %q", bad.Error)
	}

	sendUser(t, s, "跑两个慢任务")

	select {
	case d := <-boss.delivered:
		if d.Text != "configure 生效" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	for i, req := range fake.Requests {
		if hasToolResult(req, "call_0") != hasToolResult(req, "call_1") {
			t.Fatalf("configure 下发的 batch 未生效（第 %d 轮请求）: %+v", i+1, req.Messages)
		}
	}
	if !hasToolResult(*fake.LastRequest(), "call_0") || !hasToolResult(*fake.LastRequest(), "call_1") {
		t.Fatalf("最后一轮请求应含两条 [工具结果]: %+v", fake.LastRequest().Messages)
	}
}

// TestNotifyModeFromSettings：设置中心的覆盖值（tool.notify_mode=batch）在启动时拉取生效，
// 覆盖本地默认（each）——效果与 batch 模式一致：结果只成对出现。
func TestNotifyModeFromSettings(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_0", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
			{ID: "call_1", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
		}},
		{Role: "assistant", Content: "设置来了"},
	}}
	sc := startStubCenter(t)
	sc.withSettings(t, protocol.SettingsResult{Services: []protocol.ServiceSettings{{
		Service: "reasoner",
		Settings: []protocol.SettingView{{
			Key: "tool.notify_mode", Value: "batch", Default: "each", Overridden: true,
		}},
	}}})
	s, boss := startAsyncEnv(t, sc, fake, "each") // 本地是 each，中心覆盖为 batch
	sendUser(t, s, "跑两个慢任务")

	select {
	case d := <-boss.delivered:
		if d.Text != "设置来了" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	for i, req := range fake.Requests {
		if hasToolResult(req, "call_0") != hasToolResult(req, "call_1") {
			t.Fatalf("中心的 batch 覆盖未生效：第 %d 轮请求只出现一个结果: %+v", i+1, req.Messages)
		}
	}
}
