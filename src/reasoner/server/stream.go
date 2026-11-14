package server

// message 的流式答复（spec §16.3 的生产实现）：把这一轮黑板上的进展与终态以 stream 帧回给调用方，
// seq 从 1 逐块递增、最后一块 end=true，取消走传输层 cancel（spv2 client.CancelStream）。
// 注意：当前是进度/工具/终态块，不是 token 级——token 级需要 LLM 流式接口，见 IMPLEMENTATION/README。

import (
	"context"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) streamHumanMessage(ctx context.Context, call *spv2.Call, p protocol.MessagePayload) error {
	from := call.Frame.From
	if _, err := s.message(ctx, from, call.Frame.Work, call.Frame.Body); err != nil {
		return replyNativeError(call, err)
	}
	cid := p.Conversation
	if cid == "" {
		cid = "default"
	}
	st := s.boardGet(cid)
	if st == nil || st.stream == nil {
		return call.Reply(map[string]any{"status": "accepted"})
	}
	st.mu.Lock()
	sink := st.stream
	st.mu.Unlock()

	seq := int64(1)
	send := func(body map[string]any, end bool) bool {
		status := ""
		if end {
			status = spv2.StatusOK
		}
		if err := call.SendStream("s1", seq, end, body, status); err != nil {
			return false
		}
		seq += 1
		return true
	}
	ticker := time.NewTicker(120 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil // 调用方走了：不假装送达
		case delta, ok := <-sink.ch:
			if !ok {
				st.mu.Lock()
				msgs := append([]llm.Message(nil), st.messages...)
				st.mu.Unlock()
				final := finalAssistantText(msgs)
				_ = send(map[string]any{"text": final}, true)
				return call.Reply(map[string]any{"status": "accepted"})
			}
			if !send(map[string]any{"kind": "delta", "text": delta}, false) {
				return nil
			}
		case <-ticker.C:
			e := spv2.ErrCode(spv2.CodeDeadlineExceeded, "流式答复超时（120s）", true, nil)
			_ = call.SendStreamError("s1", seq, e)
			return nil
		}
	}
}

// finalAssistantText 取最后一条"纯文本 assistant"消息作为终态答案（空 = 没有）。
// 抽成纯函数便于同包单测；流式终态必须带完整答案，不能因为增量已经发过就发空。
func finalAssistantText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i -= 1 {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) == 0 {
			return msgs[i].Content
		}
	}
	return ""
}
