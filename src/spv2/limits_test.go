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
	if _, err := c.Call(context.Background(), f); !IsCode(err, CodeResourceExhausted) {
		t.Fatalf("err = %v, want resource_exhausted", err)
	}
}
