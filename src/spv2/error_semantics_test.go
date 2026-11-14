package spv2

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"testing"
	"time"
)

// spec §8 的错误码表必须是机器可读且可测的：这里的期望值照抄 §8 的 retryable 列。
func TestErrorCodeRetryableTable(t *testing.T) {
	want := map[string]bool{
		CodeInvalidArgument:   false,
		CodeNotFound:          false,
		CodeConflict:          false,
		CodeDeadlineExceeded:  true,
		CodeCancelled:         false,
		CodePermissionDenied:  false,
		CodeResourceExhausted: true,
		CodeUnavailable:       true,
		CodeInternal:          true,
		CodeStalled:           false,
		CodeNotSupported:      false,
	}
	if len(want) != 11 {
		t.Fatalf("spec §8 应有 11 个错误码，表里 %d 个", len(want))
	}
	for code, retryable := range want {
		if got := RetryableByCode(code); got != retryable {
			t.Fatalf("RetryableByCode(%q) = %v, want %v", code, got, retryable)
		}
	}
	if RetryableByCode("not_a_code") {
		t.Fatal("未知错误码不得默认 retryable")
	}
}

// spec §8：目标不可达是 unavailable（retryable=true），不是裸网络错误。
func TestDialRefusedIsUnavailable(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = Dial(ctx, "ws://"+addr+"/spv2")
	if !IsCode(err, CodeUnavailable) {
		t.Fatalf("Dial 失败错误 = %v, want unavailable", err)
	}
	var e *Error
	if !errors.As(err, &e) || !e.Retryable {
		t.Fatalf("unavailable 必须 retryable=true: %v", err)
	}
}

// handler 自己返回 ctx 错误时，码必须按 §8 映射，不能塌缩成 internal。
func TestServerMapsContextErrors(t *testing.T) {
	card := wsTestCard(wsTestMethod("cancelled"), wsTestMethod("timeout"))
	srv, url := wsStartServer(t, card)
	srv.Handle("cancelled", func(context.Context, *Call) error { return context.Canceled })
	srv.Handle("timeout", func(context.Context, *Call) error { return context.DeadlineExceeded })
	c := wsDialClient(t, url)
	for _, tc := range []struct {
		method    string
		code      string
		retryable bool
	}{
		{"cancelled", CodeCancelled, false},
		{"timeout", CodeDeadlineExceeded, true},
	} {
		f := wsCall(t, AgentRef("client"), tc.method, nil)
		_, err := c.Call(timeoutCtx(t), f)
		if !IsCode(err, tc.code) {
			t.Fatalf("%s err = %v, want %s", tc.method, err, tc.code)
		}
		var e *Error
		if !errors.As(err, &e) || e.Retryable != tc.retryable {
			t.Fatalf("%s retryable = %v, want %v", tc.method, e != nil && e.Retryable, tc.retryable)
		}
	}
}

// spec §6.6：work 已 stalled 时扣租约必须回 stalled（先 renew/resume），不得继续扣。
func TestChargeOnStalledWork(t *testing.T) {
	ctx := context.Background()
	mem := NewMemStore()
	sql, err := NewSQLiteStore(filepath.Join(t.TempDir(), "work.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer sql.Close()
	for _, tc := range []struct {
		name  string
		store WorkStore
	}{
		{"mem", mem},
		{"sqlite", sql},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := tc.store.EnsureWork(ctx, testWork()); err != nil {
				t.Fatal(err)
			}
			if err := tc.store.UpdateStatus(ctx, "w1", WorkStalled); err != nil {
				t.Fatal(err)
			}
			_, err := tc.store.Charge(ctx, "w1", Charge{ToolCalls: 1})
			if !IsCode(err, CodeStalled) {
				t.Fatalf("Charge on stalled err = %v, want stalled", err)
			}
			var e *Error
			if !errors.As(err, &e) || e.Retryable {
				t.Fatalf("stalled 必须 retryable=false: %v", err)
			}
		})
	}
}
