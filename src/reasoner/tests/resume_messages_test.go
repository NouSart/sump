package tests

// 从模型那一侧看 context 的消息链形状（与 server/resume_messages_test.go 同一件事的另一半）：
// R-026 P2c 后调用方 systemPrompt 由 reasoner 在基座前 prepend；context 回以 user 开头的
// 基座时，第一条用户消息原样到达模型，不被顶掉、不被覆盖。

import (
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

// waitFirstRequest 等第一份到达模型的请求（Fake 记的是收到的请求，与轮次一一对应）。
func waitFirstRequest(t *testing.T, fake *llm.Fake) llm.ChatRequest {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if req := fake.LastRequest(); req != nil {
			return *req
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("模型没有收到任何请求")
	return llm.ChatRequest{}
}

func TestNativeContextUserFirstReachesModelUntouched(t *testing.T) {
	// 空回复：不派工具、也不交付，只把这一轮收到的请求留在 Fake 里供断言。
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant"}}}
	s, _, mem := startAttrEnv(t, fake)
	mem.setTask(&protocol.Task{ID: "task-native-1", Name: "原生任务名"})

	in := []protocol.LLMMessage{
		{Role: "user", Content: "第一条：用户先开口"},
		{Role: "assistant", Content: "上一轮的助手发言"},
		{Role: "user", Content: "这一轮用户又说了"},
	}
	// memory 的 context 直接回这组以 user 开头的消息（模拟组装结果）。
	mem.setContextOverride(in)
	sendFrom(t, s, "stub-boss", protocol.MessagePayload{Text: "入口消息", Conversation: "c-native-user-first"})

	req := waitFirstRequest(t, fake)
	if len(req.Messages) != len(in)+1 {
		t.Fatalf("模型应看到原三条 + 一条调用方 system，实际 %d 条: %+v", len(req.Messages), req.Messages)
	}
	want := []string{"system", "user", "assistant", "user"}
	for i, role := range want {
		if req.Messages[i].Role != role {
			t.Fatalf("第 %d 条角色应为 %q，实际 %+v", i, role, rolesOfMessages(req.Messages))
		}
	}
	if !strings.Contains(req.Messages[0].Content, "单步推理器") {
		t.Fatalf("索引 0 应是 reasoner 的 systemPrompt: %q", req.Messages[0].Content)
	}
	// 一条不丢、一条不被覆盖。
	if req.Messages[1].Content != "第一条：用户先开口" {
		t.Fatalf("第一条用户消息被改写了: %q", req.Messages[1].Content)
	}
	if req.Messages[2].Content != in[1].Content || req.Messages[3].Content != in[2].Content {
		t.Fatalf("中间的轮次被弄丢了: %+v", req.Messages)
	}
}

// rolesOfMessages 把角色序列压成一行，失败时好读。
func rolesOfMessages(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}
