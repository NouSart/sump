package llm

import (
	"context"
	"sync"
)

// Fake 是测试用的 LLM 客户端：按脚本依次返回预设消息，并记录收到的请求。
// 它不是测试文件的一部分，供服务内测试与 e2e 复用。
type Fake struct {
	mu       sync.Mutex
	Replies  []Message     // 依次返回；耗尽后重复最后一条
	Usage    Usage         // 每次回复都带同一份用量（供指标测试）
	Requests []ChatRequest // 收到的请求（供断言）
}

// Chat 实现了 Client。
func (f *Fake) Chat(_ context.Context, req ChatRequest) (ChatResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, req)
	if len(f.Replies) == 0 {
		return ChatResult{Message: Message{Role: "assistant", Content: "(fake: 无剧本)"}, Usage: f.Usage}, nil
	}
	idx := len(f.Requests) - 1
	if idx >= len(f.Replies) {
		idx = len(f.Replies) - 1
	}
	return ChatResult{Message: f.Replies[idx], Usage: f.Usage}, nil
}

// CallCount 返回已处理的请求数。
func (f *Fake) CallCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.Requests)
}

// LastRequest 返回最近一次请求（无请求时为 nil）。
func (f *Fake) LastRequest() *ChatRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.Requests) == 0 {
		return nil
	}
	req := f.Requests[len(f.Requests)-1]
	return &req
}

// ChatStream 实现 StreamingClient：按内容分块回调，模拟 token 级流式。
func (f *Fake) ChatStream(ctx context.Context, req ChatRequest, onDelta func(string)) (ChatResult, error) {
	res, err := f.Chat(ctx, req)
	if err != nil {
		return ChatResult{}, err
	}
	if onDelta != nil {
		r := []rune(res.Message.Content)
		for i := 0; i < len(r); i += 2 {
			end := i + 2
			if end > len(r) {
				end = len(r)
			}
			onDelta(string(r[i:end]))
		}
	}
	return res, nil
}
