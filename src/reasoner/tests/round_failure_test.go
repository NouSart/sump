package tests

// 「这一轮没跑起来」的可见性：LLM 调用失败 → 说明送到 boss、任务不关、门户能重试。
//
// 背景：此前失败只打日志（seen 已对齐、轮末不自跳，任务静默停住），用户既没答复也没错误。
// 现在失败走 notifyRoundFailure + lastError，任务保留并由门户按 retry 重跑。
// 这条路径此前没有任何测试执行到（notifyRoundFailure 覆盖率 0）。

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

// failLLM 每次调用都失败，并记下调用次数（用来验证 retry 真的把轮次重跑了一遍）。
type failLLM struct {
	calls atomic.Int64
	err   error
}

func (f *failLLM) Chat(context.Context, llm.ChatRequest) (llm.Message, error) {
	f.calls.Add(1)
	return llm.Message{}, f.err
}

// startEnvWithLLM 起一套最小环境（中心 / boss / 记忆 / reasoner），LLM 客户端由调用方给。
func startEnvWithLLM(t *testing.T, sc *stubCenter, client llm.Client) (*server.Server, *stubBoss, *stubMemory) {
	t.Helper()
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               client,
	}, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, boss, mem
}

// callAction 以 name 的身份向 reasoner 发一个动作，返回回执。
func callAction(t *testing.T, c *protocol.Client, ctx context.Context, name, action string, in any) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(in)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, name, "reasoner", "", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatalf("%s: %v", action, err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

func TestRoundFailureNotifiesBossAndKeepsTask(t *testing.T) {
	sc := startStubCenter(t)
	// 门户名片先进名册：retry 只认门户（frontend / qq），且要求它在名册里。
	sc.mu.Lock()
	sc.cards["frontend"] = protocol.ServiceCard{Name: "frontend", Addr: "ws://127.0.0.1:1/ws", Description: "门户测试替身"}
	sc.mu.Unlock()

	fake := &failLLM{err: errors.New("LLM 连接被重置")}
	s, boss, _ := startEnvWithLLM(t, sc, fake)

	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	raw, err := protocol.EncodePayload(map[string]any{"text": "现在几点了"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	env.Boss = "stub-boss"
	resp, err := c.Call(ctx, env)
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

	var failure protocol.DeliverPayload
	select {
	case failure = <-boss.delivered:
	case <-time.After(10 * time.Second):
		t.Fatal("超时：LLM 失败后 boss 没收到任何说明（用户会以为任务还在跑）")
	}
	if !strings.Contains(failure.Text, "本轮推理失败") || !strings.Contains(failure.Text, "LLM 连接被重置") {
		t.Fatalf("失败说明应带上原因，实际: %q", failure.Text)
	}
	if !strings.Contains(failure.Text, "重试") {
		t.Fatalf("失败说明应告诉用户怎么继续，实际: %q", failure.Text)
	}
	if failure.ConversationID == "" {
		t.Fatalf("失败说明必须带会话号（门户据此重试）: %+v", failure)
	}

	// 任务没被关：reasoner 仍认这个会话有进行中的任务，且记着失败原因——retry 才会受理。
	// 轮次收尾（running=false）存在极短的窗口，重试撞上属正常，故按次轮询。
	deadline := time.Now().Add(10 * time.Second)
	var lastErr string
	retried := false
	for time.Now().Before(deadline) {
		rp := callAction(t, c, ctx, "frontend", "retry", protocol.RetryPayload{ConversationID: failure.ConversationID})
		if !rp.OK {
			lastErr = rp.Error
			time.Sleep(50 * time.Millisecond)
			continue
		}
		var rr protocol.RetryResult
		if err := protocol.DecodeRaw(rp.Data, &rr); err != nil {
			t.Fatal(err)
		}
		if !rr.Retried || rr.ConversationID != failure.ConversationID {
			t.Fatalf("重试回执不符: %+v", rr)
		}
		retried = true
		break
	}
	if !retried {
		t.Fatalf("失败后任务应保留并可重试，实际一直被拒: %s", lastErr)
	}

	// 重试真的又跑了一轮，而不只是翻了个标志位。
	deadline = time.Now().Add(5 * time.Second)
	for fake.calls.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := fake.calls.Load(); got < 2 {
		t.Fatalf("重试后应再跑一轮，LLM 调用次数 = %d", got)
	}
}
