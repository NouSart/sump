package spv2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestIdempotentCallCached(t *testing.T) {
	card := testCard()
	card.Methods = []Method{{
		Name: "echo", Shape: ShapeCall, Version: 1, Idempotent: true,
		Input: map[string]any{"type": "object"},
	}}
	srv, err := NewServer(card, WithIdempotency(NewIdempotencyCache(time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	srv.Handle("echo", func(_ context.Context, c *Call) error {
		atomic.AddInt64(&calls, 1)
		var in map[string]any
		_ = c.Frame.DecodeBody(&in)
		return c.Reply(in)
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/spv2" {
			srv.Handler().ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/spv2"
	c, err := Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mk := func() Frame {
		f, err := NewCall("w1", HumanRef("p"), ServiceRef("svc"), "echo", map[string]any{"n": 1})
		if err != nil {
			t.Fatal(err)
		}
		f.IdempotencyKey = "same-key"
		return f
	}
	var out map[string]any
	if err := c.CallDecode(context.Background(), mk(), &out); err != nil {
		t.Fatal(err)
	}
	if err := c.CallDecode(context.Background(), mk(), &out); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&calls); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
	f2 := mk()
	f2.IdempotencyKey = "other-key"
	if err := c.CallDecode(context.Background(), f2, &out); err != nil {
		t.Fatal(err)
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("handler calls = %d, want 2", got)
	}
}

// TestNonIdempotentMethodIsNotCached 是上一条的正面对照（负向用例）：
// 幂等缓存是服务卡「声明的属性」，不是全局默认。
// method 没有写 idempotent:true 时，即使带着 idempotency_key，每次也必须真的执行，
// 缓存里也不该出现条目。
// 把 server.go runCall 里 `m.Idempotent` 这个闸门删掉（即对所有 method 都套缓存），
// 本用例立刻变红。
func TestNonIdempotentMethodIsNotCached(t *testing.T) {
	card := testCard()
	card.Methods = []Method{{
		Name: "echo", Shape: ShapeCall, Version: 1, Idempotent: false,
		Input: map[string]any{"type": "object"},
	}}
	cache := NewIdempotencyCache(time.Minute)
	srv, err := NewServer(card, WithIdempotency(cache))
	if err != nil {
		t.Fatal(err)
	}
	var calls int64
	srv.Handle("echo", func(_ context.Context, c *Call) error {
		n := atomic.AddInt64(&calls, 1)
		return c.Reply(map[string]any{"n": n})
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/spv2" {
			srv.Handler().ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/spv2"
	c, err := Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	mk := func() Frame {
		f, err := NewCall("w1", HumanRef("p"), ServiceRef("svc"), "echo", map[string]any{"n": 1})
		if err != nil {
			t.Fatal(err)
		}
		// 两次用同一个 key：非幂等 method 不允许拿旧结果糊弄过去。
		f.IdempotencyKey = "same-key"
		return f
	}
	for want := int64(1); want <= 2; want++ {
		var out map[string]any
		if err := c.CallDecode(context.Background(), mk(), &out); err != nil {
			t.Fatal(err)
		}
		if got := out["n"]; got != want {
			t.Fatalf("第 %d 次拿到 body = %+v，说明结果被复放（非幂等 method 必须每次真执行）", want, out)
		}
	}
	if got := atomic.LoadInt64(&calls); got != 2 {
		t.Fatalf("handler calls = %d, want 2（未声明 idempotent 的 method 不该命中缓存）", got)
	}
	if n := cache.Len(); n != 0 {
		t.Fatalf("幂等缓存条目 = %d, want 0（未声明 idempotent 不该写缓存）", n)
	}
}
