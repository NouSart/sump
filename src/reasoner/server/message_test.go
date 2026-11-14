package server

// message 的权限矩阵（spec §6.5/§7.1）：human 必须 owner，agent 必须 none，
// runtime/owner 不能冒充 agent；agent 没有进行中的任务时必须拒绝。

import (
	"context"
	"errors"
	"io"
	"log"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func msgServerForTest() *Server {
	return New(Config{Name: "reasoner"}, log.New(io.Discard, "", 0))
}

func TestMessageAuthorityMatrix(t *testing.T) {
	s := msgServerForTest()
	raw, err := protocol.EncodePayload(protocol.MessagePayload{Text: "hi", Conversation: "c1"})
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		from spv2.Ref
		want string
	}{
		{"human 必须 owner", spv2.Ref{Kind: "human", ID: "owner"}, "authority=owner"},
		{"agent 不能带 runtime", spv2.AgentRef("a").WithAuthority(spv2.AuthorityRuntime), "authority=none"},
		{"service 不能发 message", spv2.ServiceRef("x"), "sender"},
		{"agent 没有任务", spv2.AgentRef("a"), "没有进行中的任务"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := s.message(context.Background(), tc.from, "w1", raw)
			if err == nil {
				t.Fatal("应被拒绝")
			}
			var pe *permissionError
			if !errors.As(err, &pe) {
				t.Fatalf("应 permissionError，得到 %T %v", err, err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("错误信息应含 %q: %v", tc.want, err)
			}
		})
	}
}

func TestInjectAgentMessageIntoOpenTask(t *testing.T) {
	s := msgServerForTest()
	st := s.boardOpen("c-agent", "boss", "w-agent", "task-1", "任务", nil)
	t.Cleanup(func() { s.boardClose(st) })
	out, err := s.injectAgentMessage(spv2.AgentRef("A"), protocol.MessagePayload{Text: "进展如何", Conversation: "c-agent"}, "c-agent")
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := out.(map[string]any); got["status"] != "accepted" {
		t.Fatalf("回执不符: %+v", out)
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	if len(st.messages) == 0 || !strings.Contains(st.messages[len(st.messages)-1].Content, "来自 agent:A") {
		t.Fatalf("agent 消息没注入到任务: %+v", st.messages)
	}
}

// 入口 service + sender 必须真的进人类主链（没有 memory 时失败，不能假受理）。
func TestServiceMessageWithSenderEntersHumanChain(t *testing.T) {
	s := msgServerForTest()
	raw, err := protocol.EncodePayload(protocol.MessagePayload{
		Text: "hi", Conversation: "c1",
		Sender: &protocol.Sender{Channel: "private", Owner: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.message(context.Background(), spv2.ServiceRef("entry"), "w1", raw); err == nil {
		t.Fatal("没有 memory 时人类主链应失败（不能返回假 accepted）")
	}
}
