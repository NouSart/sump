package spv2

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServerMaxBody(t *testing.T) {
	card := testCard()
	card.Limits.MaxBody = 4
	card.Methods = []Method{{Name: "big", Shape: ShapeCall, Version: 1, Input: map[string]any{"type": "object"}}}
	srv, err := NewServer(card)
	if err != nil {
		t.Fatal(err)
	}
	srv.Handle("big", func(_ context.Context, c *Call) error {
		t.Fatal("handler should not be called for oversized body")
		return nil
	})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/spv2" {
			srv.Handler().ServeHTTP(w, r)
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()
	c, err := Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http")+"/spv2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := NewCall("w1", HumanRef("p"), ServiceRef("svc"), "big", []byte("12345"))
	if err != nil {
		t.Fatal(err)
	}
	f.ContentType = ContentTypeBinary
	// 用带时限的 ctx：这条断言验的就是"服务端必须回一帧 resource_exhausted"，
	// 拿 context.Background() 等，一旦回执丢失（异变 / 回归）它会一直挂到整包超时。
	if _, err := c.Call(wsCtx(t), f); !IsCode(err, CodeResourceExhausted) {
		t.Fatalf("err = %v, want resource_exhausted", err)
	}
}
