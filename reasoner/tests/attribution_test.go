package tests

// 来源标注与静默收尾：
//   1) user_message 带 sender 时，落库正文与 LLM 上下文都带同一份标注，任务名不带标签；
//   2) 落库的 user 记录把 task_name 交给记忆侧（任务名取自正文，不取自标注）；
//   3) task_status{silent:true} → 这一轮不发出去，但把文字留在记录里（门户看得见它想过什么）。

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

// startAttrEnv 起一套最小环境（中心 / boss / 记忆 / reasoner），与 task_test 同一套替身。
func startAttrEnv(t *testing.T, fake llm.Client) (*server.Server, *stubBoss, *stubMemory) {
	t.Helper()
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	mem.setTask(&protocol.Task{ID: "task-1", Name: "旧名字", Status: "active"})

	logger := log.New(os.Stdout, "[attr-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, boss, mem
}

// sendGroupUser 向 reasoner 发一条带来源标注的 user_message。
func sendGroupUser(t *testing.T, s *server.Server, text string, sender protocol.Sender) {
	t.Helper()
	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(protocol.UserMessagePayload{
		Text: text, ConversationID: "qq:group:67890", Sender: &sender,
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "qq", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	// boss 是这批测试的替身入口（deliver 的落点）；会话号保持在群里。
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

// waitStoreContaining 等一条内容里含 marker 的 store 记录。
func waitStoreContaining(t *testing.T, mem *stubMemory, marker string) protocol.StorePayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, sp := range mem.Stores() {
			if strings.Contains(sp.Content, marker) {
				return sp
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待含 %q 的 store 记录超时：%+v", marker, mem.Stores())
	return protocol.StorePayload{}
}

// TestUserMessageCarriesSenderLabel 标注进正文（记忆 / 模型 / 门户同一句话），任务名不带标签。
func TestUserMessageCarriesSenderLabel(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_1", `{"status":"done"}`)}},
		{Role: "assistant", Content: "好的"},
	}}
	s, boss, mem := startAttrEnv(t, fake)
	sendGroupUser(t, s, "今天几点开会", protocol.Sender{
		Channel: "group", ID: "12345", Name: "张三",
		GroupID: "67890", GroupName: "编程交流", GroupSize: 12,
	})

	saved := waitStoreContaining(t, mem, "今天几点开会")
	if !strings.HasPrefix(saved.Content, "[群聊 · ") {
		t.Fatalf("落库正文应带来源标注: %q", saved.Content)
	}
	for _, want := range []string{"群「编程交流」(67890)", "12 人可见", "张三(12345)", "非主人", "未@我"} {
		if !strings.Contains(saved.Content, want) {
			t.Fatalf("标注缺 %q: %q", want, saved.Content)
		}
	}
	// 任务名取自正文而不是标签（任务名会显示在界面会话树里）。
	if saved.TaskName != "今天几点开会" {
		t.Fatalf("task_name 应为正文首行，实际 %q", saved.TaskName)
	}
	// 同一条也随上下文托管给记忆（store 与 recall 并发，两条路径都得给）。
	recalls := mem.RecallContexts()
	if len(recalls) == 0 {
		t.Fatal("没有收到 recall")
	}
	if recalls[0].TaskName != "今天几点开会" {
		t.Fatalf("recall 的 task_name 不符（store 与 recall 会抢着建任务）: %q", recalls[0].TaskName)
	}
	if saved.ConversationID != "qq:group:67890" {
		t.Fatalf("会话号不符: %q", saved.ConversationID)
	}

	// 模型看到的是同一份标注（不是裸文本），否则它无从判断该不该在群里说话。
	last := fake.LastRequest()
	joined := ""
	if last != nil {
		for _, m := range last.Messages {
			joined += m.Content + "\n"
		}
	}
	if !strings.Contains(joined, "[群聊 · ") {
		t.Fatalf("LLM 上下文里应带来源标注: %q", joined)
	}

	select {
	case d := <-boss.delivered:
		if d.ConversationID != "qq:group:67890" || d.Text != "好的" {
			t.Fatalf("交付不符: %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}
}

// TestSilentCloseDoesNotDeliver 群里判断不该插话：这一轮不发出去，但文字留在记录里。
func TestSilentCloseDoesNotDeliver(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_1", `{"status":"done","silent":true,"note":"这轮不该插话"}`)}},
		{Role: "assistant", Content: "（这句不该出现在群里）"},
	}}
	s, boss, mem := startAttrEnv(t, fake)
	sendGroupUser(t, s, "群里闲聊", protocol.Sender{
		Channel: "group", ID: "12345", GroupID: "67890", GroupSize: 5, Mentioned: false,
	})

	// 静默：boss 收不到任何 deliver。
	select {
	case d := <-boss.delivered:
		t.Fatalf("静默轮不该交付: %+v", d)
	case <-time.After(2 * time.Second):
	}

	// 文字仍留在记录里（门户能看见"它想过什么"），并标成过程轨迹而不是对谁说的话。
	saved := waitStoreContaining(t, mem, "这句不该出现在群里")
	if saved.Kind != "reasoning" || saved.Role != "assistant" {
		t.Fatalf("静默记录应记为 reasoning: role=%q kind=%q", saved.Role, saved.Kind)
	}
	if !strings.Contains(saved.Content, "静默") {
		t.Fatalf("静默记录应说明没有发送: %q", saved.Content)
	}
}

// TestSilentVoidedByNewToolCall 静默随判定一起作废：同一轮里既判了静默、又派了工具，
// 工具跑完后的收尾不能再沿用那个静默——否则"有新进展"的答复会被悄悄吞掉。
func TestSilentVoidedByNewToolCall(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			{ID: "call_slow", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
			taskStatusCall("call_status", `{"status":"continue","silent":true}`),
		}},
		{Role: "assistant", Content: "工具跑完了"},
	}}
	// 用没有任务的环境：判定门禁不介入，静默标记是唯一决定"发不发"的东西。
	s, boss := startAsyncEnv(t, startStubCenter(t), fake, "")
	sendGroupUser(t, s, "跑个慢任务", protocol.Sender{Channel: "group", ID: "1", GroupID: "67890", GroupSize: 5})

	select {
	case d := <-boss.delivered:
		if d.Text != "工具跑完了" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：派发新工具后静默应作废，答复必须交付")
	}
}

func TestSilentIsNotStickyAcrossMessages(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{
		// 第一轮：静默收尾
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_1", `{"status":"continue","silent":true}`)}},
		{Role: "assistant", Content: "（第一轮不该发）"},
		// 第二轮：正常回答
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_2", `{"status":"done"}`)}},
		{Role: "assistant", Content: "这句要发"},
	}}
	s, boss, _ := startAttrEnv(t, fake)
	sendGroupUser(t, s, "第一条", protocol.Sender{Channel: "group", ID: "1", GroupID: "67890", GroupSize: 5})

	// 等第一轮静默结束（LLM 被调 2 次：判定 + 收尾）。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && fake.CallCount() < 2 {
		time.Sleep(50 * time.Millisecond)
	}

	sendGroupUser(t, s, "第二条", protocol.Sender{Channel: "group", ID: "1", GroupID: "67890", GroupSize: 5})
	select {
	case d := <-boss.delivered:
		if d.Text == "（第一轮不该发）" {
			t.Fatal("静默状态被读成了粘性：第一轮的文本被发出去了")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：第二轮应正常交付")
	}
}
