package tests

// 从模型那一侧看 resume 的消息链形状（与 server/resume_messages_test.go 同一件事的另一半）：
// 原生 resume 送来以 user 开头的上下文时，任务提示插在索引 1，第一条用户消息原样到达模型——
// 既没有被顶掉，也没有被丢掉。审计报告认为这里「首条被无声覆盖」，这条用例把实际形状钉住。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
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

func TestNativeResumeUserFirstReachesModelUntouched(t *testing.T) {
	// 空回复：不派工具、也不交付，只把这一轮收到的请求留在 Fake 里供断言。
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant"}}}
	s, _, _ := startAttrEnv(t, fake)

	in := []protocol.Message{
		{Role: "user", Content: "第一条：用户先开口"},
		{Role: "assistant", Content: "上一轮的助手发言"},
		{Role: "user", Content: "这一轮用户又说了"},
	}
	body := protocol.ResumePayload{Context: protocol.TaskContext{
		ConversationID: "c-native-user-first",
		Messages:       in,
		Task:           &protocol.Task{ID: "task-native-1", Name: "原生任务名"},
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialNative(t, s)
	f, err := spv2.NewCall("w-native-user-first", spv2.ServiceRef("memory"), spv2.ServiceRef("reasoner"), "resume", body)
	if err != nil {
		t.Fatal(err)
	}
	var resumed struct {
		Status string `msgpack:"status"`
	}
	if err := c.CallDecode(ctx, f, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "accepted" {
		t.Fatalf("resume 未被受理: %+v", resumed)
	}

	req := waitFirstRequest(t, fake)
	if len(req.Messages) != len(in)+1 {
		t.Fatalf("模型应看到原三条 + 一条任务提示，实际 %d 条: %+v", len(req.Messages), req.Messages)
	}
	want := []string{"user", "system", "assistant", "user"}
	for i, role := range want {
		if req.Messages[i].Role != role {
			t.Fatalf("第 %d 条角色应为 %q，实际 %+v", i, role, rolesOfMessages(req.Messages))
		}
	}
	// 一条不丢、一条不被覆盖（第 0 条就是用户那句话本身，不是任务提示）。
	if req.Messages[0].Content != "第一条：用户先开口" {
		t.Fatalf("第一条用户消息被改写了: %q", req.Messages[0].Content)
	}
	if req.Messages[2].Content != in[1].Content || req.Messages[3].Content != in[2].Content {
		t.Fatalf("中间的轮次被弄丢了: %+v", req.Messages)
	}
	if !strings.Contains(req.Messages[1].Content, "当前任务：《原生任务名》（id=task-native-1）") {
		t.Fatalf("索引 1 应是任务提示: %q", req.Messages[1].Content)
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
