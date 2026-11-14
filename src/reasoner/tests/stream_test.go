package tests

// spec §16.3：message 的流式答复（生产路径）——多块、seq 递增、end=true、终态带最终答案。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func TestMessageStreamsProgressAndEnd(t *testing.T) {
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "流式答复"}}}
	s, _, _ := startAttrEnv(t, fake)
	c := dialNative(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef("stub-boss"), spv2.ServiceRef("reasoner"), "message", protocol.MessagePayload{
		Text: "说一句", Conversation: "conv-stream", Stream: true,
		Sender: &protocol.Sender{Channel: "private", Name: "boss", Owner: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	ch, err := c.Stream(ctx, call)
	if err != nil {
		t.Fatalf("发起流式 message 失败: %v", err)
	}
	var seqs []int64
	var deltas []string
	ended := false
	final := ""
	for f := range ch {
		if f.Type != spv2.TypeStream || f.Stream == nil {
			continue
		}
		seqs = append(seqs, f.Stream.Seq)
		if !f.Stream.End {
			var chunk struct {
				Kind string `msgpack:"kind"`
				Text string `msgpack:"text"`
			}
			_ = f.DecodeBody(&chunk)
			if chunk.Kind == "delta" {
				deltas = append(deltas, chunk.Text)
			}
		}
		if f.Stream.End {
			ended = true
			var body struct {
				Text string `msgpack:"text"`
			}
			_ = f.DecodeBody(&body)
			final = body.Text
		}
	}
	if len(seqs) < 2 {
		t.Fatalf("应有至少 2 块（进展+终态），实际 seq=%v", seqs)
	}
	if !ended {
		t.Fatal("最后一块必须是 end")
	}
	for i := 1; i < len(seqs); i += 1 {
		if seqs[i] != seqs[i-1]+1 {
			t.Fatalf("seq 必须逐 1 递增: %v", seqs)
		}
	}
	if final != "流式答复" {
		t.Fatalf("终态应带最终答案，实际 %q", final)
	}
	// token 级：增量必须多于一块，且拼起来就是完整答案（不是只发终态）。
	if len(deltas) < 2 {
		t.Fatalf("应收到多块 token 增量，实际 %v", deltas)
	}
	if !strings.Contains(strings.Join(deltas, ""), "流式答复") {
		t.Fatalf("增量里应包含模型输出: %v", deltas)
	}
}
