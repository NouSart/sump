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
		if r.URL.Path == "/ws" {
			srv.Handler().ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
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
