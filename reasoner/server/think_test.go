package server

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
)

// TestActionThink 验证单步推理动作：一次补全、原样回文本、上限生效、空提示词拒绝。
func TestActionThink(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "一句话答案"}}}
	s := &Server{llm: fake, logger: log.New(os.Stdout, "[think-test] ", 0), cfg: Config{Name: "reasoner"}}

	in, err := protocol.EncodePayload(protocol.ThinkPayload{System: "你是助手", Prompt: "给个答案", MaxTokens: 99_999, Model: "cheap-model"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.actionThink(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Action: "think", Input: in})
	if err != nil {
		t.Fatalf("think 失败: %v", err)
	}
	if got := out.(protocol.ThinkResult).Text; got != "一句话答案" {
		t.Fatalf("回文本不符: %q", got)
	}

	req := fake.LastRequest()
	if req == nil {
		t.Fatal("假客户端没收到请求")
	}
	if len(req.Messages) != 2 || req.Messages[0].Role != "system" || req.Messages[1].Role != "user" {
		t.Fatalf("system + user 两条消息不符: %+v", req.Messages)
	}
	// 上限是硬夹紧的：填一个天文数字不能原样发出去。
	if req.MaxTokens != thinkMaxTokens {
		t.Fatalf("max_tokens 应夹到 %d，实际 %d", thinkMaxTokens, req.MaxTokens)
	}
	// 模型可以按调用覆盖（"最小不思考"就是靠这个指一个非推理模型）。
	if req.Model != "cheap-model" {
		t.Fatalf("model 覆盖没传下去: %q", req.Model)
	}
	if len(req.Tools) != 0 {
		t.Fatalf("think 不带工具: %+v", req.Tools)
	}

	// 空提示词：明确拒绝，不是发一个空气请求出去。
	empty, _ := protocol.EncodePayload(protocol.ThinkPayload{Prompt: "   "})
	if _, err := s.actionThink(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: empty}); err == nil {
		t.Fatal("空提示词应报错")
	}

	// 未配置 LLM：如实报错，调用方自己决定降级。
	bare := &Server{logger: log.New(os.Stdout, "[think-test] ", 0)}
	ok, _ := protocol.EncodePayload(protocol.ThinkPayload{Prompt: "x"})
	if _, err := bare.actionThink(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: ok}); err == nil {
		t.Fatal("未配置 LLM 时应报错")
	}
}

// TestActionThinkRejectsHugePrompt 验证提示词硬上限：误拼的循环不该把账单打飞。
func TestActionThinkRejectsHugePrompt(t *testing.T) {
	s := &Server{llm: &llm.Fake{}, logger: log.New(os.Stdout, "[think-test] ", 0)}
	in, err := protocol.EncodePayload(protocol.ThinkPayload{Prompt: strings.Repeat("长", thinkMaxPromptChars+1)})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.actionThink(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Input: in})
	if err == nil || !strings.Contains(err.Error(), "太长") {
		t.Fatalf("超长提示词应被拒绝: %v", err)
	}
}

// TestApproxTokens 验证 token 估算的量级：中文按 1 字 1 token，英文按 4 字符 1 token。
// 它只用于"留多留少"，估准不是目标，估崩才是问题。
func TestApproxTokens(t *testing.T) {
	if got := approxTokens(strings.Repeat("中", 100)); got < 90 || got > 110 {
		t.Fatalf("100 个汉字应约 100 token，实际 %d", got)
	}
	if got := approxTokens(strings.Repeat("a", 400)); got < 90 || got > 110 {
		t.Fatalf("400 个 ASCII 应约 100 token，实际 %d", got)
	}
	if got := approxTokens(""); got != 0 {
		t.Fatalf("空串应是 0，实际 %d", got)
	}
}
