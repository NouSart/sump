package service

// 骨架层的两条隔离承诺（每个服务都 embed src/service，所以这两件事是全网待遇）：
//
//   - 原生 handler panic：调用方必须收到 code=internal 的回执而不是空等到超时，
//     并且计数进 sump_panics_total；
//   - 入站帧解不开：计数 sump_dropped_total{reason="undecodable"}，并且**落到本服务自己的 Logger**
//     （buildSPv2 用 WithLogger 接线；接不上就只会进进程默认 stderr，谁都看不出是谁丢的）。
//
// 对端是本机 httptest + gorilla WS，不碰外网。

import (
	"bytes"
	"context"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/spv2"
)

// syncLogger 是并发安全的日志缓冲（骨架日志来自服务端 goroutine）。
type syncLogger struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (l *syncLogger) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *syncLogger) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// waitSkeletonCounter 轮询骨架服务端指标，直到某个 name+labels 组合到位。
func waitSkeletonCounter(t *testing.T, s *Service, name string, match func(spv2.Metric) bool) spv2.Metric {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		for _, m := range s.spv2Srv.Metrics().Snapshot() {
			if m.Name == name && match(m) {
				return m
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("等待指标 %s 超时: %+v", name, s.spv2Srv.Metrics().Snapshot())
		}
		time.Sleep(time.Millisecond)
	}
}

// TestSkeletonPanickingHandlerRepliesInternal 钉住：handler panic 不掀进程、不吞回执。
func TestSkeletonPanickingHandlerRepliesInternal(t *testing.T) {
	lg := &syncLogger{}
	s := New(Config{Name: "victim", Logger: log.New(lg, "", 0), SecretFile: "-"})
	s.HandleNative(spv2.Method{Name: "boom", Tool: true}, func(context.Context, *spv2.Call) error {
		panic("原生处理方炸了")
	})
	s.HandleNative(spv2.Method{Name: "echo", Tool: true}, func(_ context.Context, call *spv2.Call) error {
		return call.Reply("ok")
	})
	server := startNativeService(t, s)

	client, err := spv2.Dial(context.Background(), nativeURL(server))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame, err := spv2.NewCall("w-panic", spv2.ServiceRef("caller"), spv2.ServiceRef("victim"), "boom", nil)
	if err != nil {
		t.Fatalf("NewCall: %v", err)
	}
	_, err = client.Call(ctx, frame)
	if !spv2.IsCode(err, spv2.CodeInternal) {
		t.Fatalf("panic 的回执 err = %v, want code=%s（调用方不能空等）", err, spv2.CodeInternal)
	}
	// 计数与日志都到位，而且日志走的是这个服务自己的 Logger。
	waitSkeletonCounter(t, s, "sump_panics_total", func(m spv2.Metric) bool {
		return m.Labels["method"] == "boom" && m.Value >= 1
	})
	if !strings.Contains(lg.String(), "原生处理方炸了") {
		t.Fatalf("panic 没有落进服务自己的日志: %q", lg.String())
	}
	// 同一条链路仍然可用：panic 不该带走骨架服务端。
	echoFrame, err := spv2.NewCall("w-echo", spv2.ServiceRef("caller"), spv2.ServiceRef("victim"), "echo", nil)
	if err != nil {
		t.Fatalf("NewCall(echo): %v", err)
	}
	if _, err := client.Call(ctx, echoFrame); err != nil {
		t.Fatalf("panic 之后同一连接不可用: %v", err)
	}
}

// TestSkeletonUndecodableFrameCountedAndLogged 钉住：坏帧不再静默 return，既计数也进本服务日志。
func TestSkeletonUndecodableFrameCountedAndLogged(t *testing.T) {
	lg := &syncLogger{}
	s := New(Config{Name: "receiver", Logger: log.New(lg, "", 0), SecretFile: "-"})
	s.HandleNative(spv2.Method{Name: "echo", Tool: true}, func(_ context.Context, call *spv2.Call) error {
		return call.Reply("ok")
	})
	server := startNativeService(t, s)

	conn, _, err := websocket.DefaultDialer.Dial(nativeURL(server), nil)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer conn.Close()
	junk := []byte("this is definitely not an spv2 frame")
	if _, err := spv2.Unmarshal(junk); err == nil {
		t.Fatal("用例前提不成立：这串字节竟然能解成帧")
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, junk); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}

	waitSkeletonCounter(t, s, "sump_dropped_total", func(m spv2.Metric) bool {
		return m.Labels["reason"] == "undecodable" && m.Value >= 1
	})
	if !strings.Contains(lg.String(), "丢弃入站帧") || !strings.Contains(lg.String(), "undecodable") {
		t.Fatalf("丢弃帧没有落进服务自己的日志: %q", lg.String())
	}
}
