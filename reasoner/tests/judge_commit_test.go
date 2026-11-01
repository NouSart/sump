package tests

// 判定的落库时机：task_status 只是「这一轮的想法」，真正交给记忆服务的是**收尾那一刻**。
//
// 修复前的反例：模型在同一轮里判了 done、又派发了新工具——判定当场作废（新进展要重新决定），
// 但任务已经在记忆里被标成 done；随后模型改判 continue 也打不开它（task_resume 拒绝 done、
// task_rename 不改状态），下半程的工作全挂在一个"已完成"的任务里。

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

// TestJudgementCommittedAtCloseOnly 被判废的 done 不落库；收尾时按**最后一次**判定记账。
func TestJudgementCommittedAtCloseOnly(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	mem.setTask(&protocol.Task{ID: "task-9", Name: "旧标题", Status: "active"})
	_ = startStubSlow(t, sc) // slow__work：真工具，派发它会让本轮判定作废

	fake := &llm.Fake{Replies: []llm.Message{
		// 第一轮：同一轮里既判 done、又派新工具。
		{Role: "assistant", ToolCalls: []llm.ToolCall{
			taskStatusCall("call_1", `{"status":"done","title":"误判的完成"}`),
			{ID: "call_2", Type: "function", Function: llm.FunctionCall{Name: "slow__work", Arguments: "{}"}},
		}},
		// 第二轮（工具结果回来）：改判 continue，并给一个更像人写的标题。
		{Role: "assistant", ToolCalls: []llm.ToolCall{taskStatusCall("call_3", `{"status":"continue","title":"还在做"}`)}},
		// 第三轮：交付。
		{Role: "assistant", Content: "先做到这一步。"},
	}}

	logger := log.New(os.Stdout, "[judge-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name: "reasoner", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond, Memory: "memory", LLM: fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)
	raw, _ := protocol.EncodePayload(map[string]any{"text": "开始做"})
	req, _ := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	req.Boss = "stub-boss"
	if _, err := c.Call(ctx, req); err != nil {
		t.Fatal(err)
	}

	select {
	case d := <-boss.delivered:
		if d.Text != "先做到这一步。" {
			t.Fatalf("交付内容不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	// 收尾时按最后一次判定办：continue → 只改名，**不完结**。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(mem.Renamed()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	ren := mem.Renamed()
	if len(ren) != 1 || ren[0].TaskID != "task-9" || ren[0].Name != "还在做" {
		t.Fatalf("task_rename 不符（应只落最后一次判定给的标题）: %+v", ren)
	}
	if fin := mem.Finished(); len(fin) != 0 {
		t.Fatalf("被判废的 done 不该落库（任务被错误终结）: %+v", fin)
	}
}
