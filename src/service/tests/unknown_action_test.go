package tests

// 协议红线：未知动作必须回 {ok:false, error}，不能静默丢弃或当成功。
// 直接向运行中的骨架发一条未注册动作，验证错误里带动作名（排查时能定位）。

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

func TestUnknownActionRejected(t *testing.T) {
	sc := startStubCenter(t)
	s := service.New(service.Config{
		Name:              "svc-unknown",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: time.Second,
		Logger:            log.New(io.Discard, "", 0),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	c, err := spv2.Dial(context.Background(), s.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	frame, err := spv2.NewCall("trace-unknown", spv2.ServiceRef("caller"), spv2.ServiceRef("svc-unknown"), "no_such_action", nil)
	if err != nil {
		t.Fatal(err)
	}
	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	_, err = c.Call(callCtx, frame)
	if err == nil {
		t.Fatal("未知动作不得返回成功")
	}
	if !strings.Contains(err.Error(), "unknown method") || !strings.Contains(err.Error(), "no_such_action") {
		t.Fatalf("错误应指明未知动作: %v", err)
	}
}
