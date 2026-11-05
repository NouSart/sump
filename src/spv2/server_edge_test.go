package spv2

// 变异测试暴露的边界：最大 body 的等号/零值、handler 不回复、幂等错误缓存、inflight gauge 回收。
// 这些用例只补断言，不改业务语义。

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestServerMaxBodyBoundary(t *testing.T) {
	cases := []struct {
		name    string
		maxBody int64
		body    string
		wantErr bool
	}{
		{name: "equal", maxBody: 4, body: "1234"},
		{name: "over", maxBody: 4, body: "12345", wantErr: true},
		{name: "zero_unlimited", maxBody: 0, body: "12345"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			card := wsTestCard(wsTestMethod("echo"))
			card.Limits.MaxBody = tc.maxBody
			srv, url := wsStartServer(t, card)
			srv.Handle("echo", func(_ context.Context, c *Call) error { return c.Reply("ok") })
			c := wsDialClient(t, url)
			f, err := NewCall("w1", HumanRef("p"), ServiceRef(wsTestService), "echo", []byte(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			f.ContentType = ContentTypeBinary
			_, err = c.Call(context.Background(), f)
			if tc.wantErr {
				if !IsCode(err, CodeResourceExhausted) {
					t.Fatalf("err = %v, want resource_exhausted", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v", err)
			}
		})
	}
}

func TestServerHandlerDidNotReply(t *testing.T) {
	card := wsTestCard(wsTestMethod("silent"))
	srv, url := wsStartServer(t, card)
	srv.Handle("silent", func(context.Context, *Call) error { return nil })
	c := wsDialClient(t, url)
	f, err := NewCall("w1", HumanRef("p"), ServiceRef(wsTestService), "silent", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Call(context.Background(), f)
	if !IsCode(err, CodeInternal) || !strings.Contains(err.Error(), "handler did not reply") {
		t.Fatalf("err = %v, want internal handler did not reply", err)
	}
}

func TestServerIdempotentErrorReplyCached(t *testing.T) {
	card := wsTestCard(wsTestMethod("idem"))
	card.Methods[0].Idempotent = true
	srv, url := wsStartServer(t, card, WithIdempotency(NewIdempotencyCache(time.Hour)))
	var calls atomic.Int32
	srv.Handle("idem", func(_ context.Context, c *Call) error {
		calls.Add(1)
		return c.ReplyError(CodeInvalidArgument, "bad", false, nil)
	})
	c := wsDialClient(t, url)
	for i := 0; i < 2; i++ {
		f, err := NewCall("w1", HumanRef("p"), ServiceRef(wsTestService), "idem", nil)
		if err != nil {
			t.Fatal(err)
		}
		f.IdempotencyKey = "same"
		if _, err := c.Call(context.Background(), f); !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("call %d err = %v, want invalid_argument", i, err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (second should hit cache)", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		var badCode, okCode float64
		for _, m := range srv.Metrics().Snapshot() {
			if m.Name != "sump_requests_total" || m.Labels["method"] != "idem" {
				continue
			}
			switch m.Labels["code"] {
			case CodeInvalidArgument:
				badCode = m.Value
			case StatusOK:
				okCode = m.Value
			}
		}
		if badCode >= 2 && okCode == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("metrics invalid_argument=%v ok=%v, want 2 and 0", badCode, okCode)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestServerUnknownAndFailedEventMetrics(t *testing.T) {
	card := wsTestCard(wsTestMethod("ok"))
	srv, url := wsStartServer(t, card)
	srv.Handle("boom", func(context.Context, *Call) error { return context.DeadlineExceeded })
	c := wsDialClient(t, url)

	unknown, err := NewEvent("w1", ServiceRef(wsTestService), ServiceRef(wsTestService), "unknown", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(unknown); err != nil {
		t.Fatal(err)
	}
	failed, err := NewEvent("w1", ServiceRef(wsTestService), ServiceRef(wsTestService), "boom", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(failed); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		ms := srv.Metrics().Snapshot()
		if counterValue(ms, "sump_unknown_event_total", "unknown") >= 1 && counterValue(ms, "sump_event_errors_total", "boom") >= 1 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("event metrics missing: unknown=%v failed=%v", counterValue(ms, "sump_unknown_event_total", "unknown"), counterValue(ms, "sump_event_errors_total", "boom"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func counterValue(ms []Metric, name, method string) float64 {
	for _, m := range ms {
		if m.Name == name && m.Labels["method"] == method {
			return m.Value
		}
	}
	return 0
}

func TestServerInflightGaugeReclaimed(t *testing.T) {
	card := wsTestCard(wsTestMethod("slow"))
	srv, url := wsStartServer(t, card)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	srv.Handle("slow", func(_ context.Context, c *Call) error {
		entered <- struct{}{}
		<-release
		return c.Reply("ok")
	})
	c := wsDialClient(t, url)
	f, err := NewCall("w1", HumanRef("p"), ServiceRef(wsTestService), "slow", nil)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.Call(context.Background(), f)
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	if got := gaugeValue(srv.Metrics().Snapshot(), "sump_inflight_requests", "slow"); got != 1 {
		t.Fatalf("inflight while running = %v, want 1", got)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if got := gaugeValue(srv.Metrics().Snapshot(), "sump_inflight_requests", "slow"); got == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("inflight not reclaimed: %v", gaugeValue(srv.Metrics().Snapshot(), "sump_inflight_requests", "slow"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func gaugeValue(ms []Metric, name, method string) float64 {
	for _, m := range ms {
		if m.Name == name && m.Labels["method"] == method {
			return m.Value
		}
	}
	return 0
}

func TestServerHelloValidation(t *testing.T) {
	card := wsTestCard(wsTestMethod("x"))
	_, url := wsStartServer(t, card)
	c := wsDialClient(t, url)
	cases := []any{
		HelloRequest{Protocol: "spv2.0"},
		[]byte("bad"),
	}
	for _, body := range cases {
		f, err := NewCall("w-hello", HumanRef("p"), ServiceRef(wsTestService), "hello", body)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := c.Call(context.Background(), f); !IsCode(err, CodeInvalidArgument) {
			t.Fatalf("hello %T err = %v, want invalid_argument", body, err)
		}
	}
}

func TestServerMaxMessageBytesCloses(t *testing.T) {
	card := wsTestCard(wsTestMethod("echo"))
	srv, url := wsStartServer(t, card, WithMaxMessageBytes(64))
	srv.Handle("echo", func(_ context.Context, c *Call) error { return c.Reply("ok") })
	c := wsDialClient(t, url)
	f, err := NewCall("w-big", HumanRef("p"), ServiceRef(wsTestService), "echo", strings.Repeat("x", 256))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(context.Background(), f); err == nil {
		t.Fatal("oversized frame should close the server connection")
	}
}
