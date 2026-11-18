package spv2

import (
	"sync"
	"time"
)

// IdempotencyCache 是可选的幂等缓存。只缓存 unary reply；stream 不缓存。
type IdempotencyCache struct {
	mu       sync.Mutex
	ttl      time.Duration
	entries  map[string]idemEntry
	inflight map[string]chan struct{}
}

type idemEntry struct {
	frame   Frame
	expires time.Time
}

// NewIdempotencyCache 创建缓存；ttl<=0 用 1h。
func NewIdempotencyCache(ttl time.Duration) *IdempotencyCache {
	if ttl <= 0 {
		ttl = time.Hour
	}
	return &IdempotencyCache{ttl: ttl, entries: map[string]idemEntry{}, inflight: map[string]chan struct{}{}}
}

func idemKeyOf(f Frame) string {
	return f.From.Kind + "\x00" + f.From.ID + "\x00" + f.Method + "\x00" + f.IdempotencyKey
}

func (c *IdempotencyCache) get(key string) (Frame, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[key]
	if !ok {
		return Frame{}, false
	}
	if time.Now().After(e.expires) {
		delete(c.entries, key)
		return Frame{}, false
	}
	return e.frame, true
}

func (c *IdempotencyCache) put(key string, f Frame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f.Body = append([]byte(nil), f.Body...)
	c.entries[key] = idemEntry{frame: f, expires: time.Now().Add(c.ttl)}
}

// begin 登记一个在途幂等键：leader=true 的执行，其余等它完成再读缓存。
func (c *IdempotencyCache) begin(key string) (wait <-chan struct{}, leader bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ch, ok := c.inflight[key]; ok {
		return ch, false
	}
	ch := make(chan struct{})
	c.inflight[key] = ch
	return ch, true
}

// finish 关闭在途登记（无论成功失败都要调，否则重复请求会一直等）。
func (c *IdempotencyCache) finish(key string) {
	c.mu.Lock()
	ch, ok := c.inflight[key]
	if ok {
		delete(c.inflight, key)
	}
	c.mu.Unlock()
	if ok {
		close(ch)
	}
}

// Len 返回当前缓存条数（测试/观测用）。
func (c *IdempotencyCache) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}
