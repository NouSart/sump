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
	Requests []ChatRequest // 收到的请求（供断言）
}

// Chat 实现了 Client。
func (f *Fake) Chat(_ context.Context, req ChatRequest) (Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, req)
	if len(f.Replies) == 0 {
		return Message{Role: "assistant", Content: "(fake: 无剧本)"}, nil
	}
	idx := len(f.Requests) - 1
	if idx >= len(f.Replies) {
		idx = len(f.Replies) - 1
	}
	return f.Replies[idx], nil
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
