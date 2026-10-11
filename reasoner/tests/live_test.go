package tests

// M2.5 live：真实 DeepSeek 冒烟测试（显式开启才运行，避免日常测试意外花钱/联网）。
//
// 运行方式（PowerShell）：
//
//	$env:DEEPSEEK_API_KEY = 'sk-...'   # 你的 key（切勿写入任何文件）
//	$env:SUMP_LIVE_LLM = '1'
//	go test ./tests/ -run TestLiveDeepSeek -v -count=1
//
// 可选：$env:SUMP_LLM_MODEL 指定模型（默认 deepseek-chat）。

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

func TestLiveDeepSeek(t *testing.T) {
	if os.Getenv("SUMP_LIVE_LLM") == "" {
		t.Skip("未设置 SUMP_LIVE_LLM=1，跳过真实 LLM 测试")
	}
	key := os.Getenv("DEEPSEEK_API_KEY")
	if key == "" {
		t.Skip("未设置 DEEPSEEK_API_KEY，跳过")
	}
	model := os.Getenv("SUMP_LLM_MODEL")
	if model == "" {
		model = "deepseek-chat"
	}

	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)

	logger := log.New(os.Stdout, "[live] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 5 * time.Second,
		Memory:            "memory",
		LLM:               llm.NewDeepSeekClient("https://api.deepseek.com", key, model),
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(map[string]any{
		"text": "请先调用 stub-center__ping 工具，然后用一句话告诉我工具返回了什么。",
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "live-test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
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

	select {
	case d := <-boss.delivered:
		t.Logf("boss 收到交付: %q", d.Text)
		if d.Text == "" {
			t.Fatal("交付文本为空")
		}
	case <-time.After(120 * time.Second):
		t.Fatal("超时：boss 未收到 deliver（检查网络 / key / 模型名）")
	}
}
