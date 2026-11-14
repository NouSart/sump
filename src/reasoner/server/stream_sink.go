package server

// streamSink 把 board 轮次里的 token 增量转给 message 的流式答复。
// 无流式请求时任务没有 sink（nil），非流式路径完全不受影响。

import "sync"

type streamSink struct {
	mu     sync.Mutex
	ch     chan string
	closed bool
}

func newStreamSink() *streamSink { return &streamSink{ch: make(chan string, 64)} }

func (k *streamSink) push(delta string) {
	if delta == "" {
		return
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.closed {
		return
	}
	select {
	case k.ch <- delta:
	default: // 消费端太慢：丢增量而不是阻塞整轮推理（终态仍带完整答案）
	}
}

func (k *streamSink) close() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if !k.closed {
		k.closed = true
		close(k.ch)
	}
}
