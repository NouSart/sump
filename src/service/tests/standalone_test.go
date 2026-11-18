package tests

// 独立模式：内部服务只监听 /spv2，不连接设置中心。
// 验证三件事：空 Center 也能启动、/readyz 不因缺中心而恒失败、静态 Peers 能解析依赖。

import (
	"context"
	"io"
	"log"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

func TestStandaloneStartsWithoutCenter(t *testing.T) {
	s := service.New(service.Config{
		Name:       "svc-standalone",
		Listen:     "127.0.0.1:0",
		Standalone: true,
		Logger:     log.New(io.Discard, "", 0),
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

	frame, err := spv2.NewCall("trace-standalone", spv2.ServiceRef("caller"), spv2.ServiceRef("svc-standalone"), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer callCancel()
	if _, err := c.Call(callCtx, frame); err != nil {
		t.Fatalf("独立模式应能响应 hello: %v", err)
	}

	base := strings.Replace(s.WsURL(), "ws://", "http://", 1)
	base = strings.TrimSuffix(base, "/spv2")
	resp, err := http.Get(base + "/readyz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("独立模式 /readyz 应为 200，实际 %d", resp.StatusCode)
	}
}

func TestStandaloneResolvesPeers(t *testing.T) {
	s := service.New(service.Config{
		Name:       "svc-gateway",
		Listen:     "127.0.0.1:0",
		Standalone: true,
		Peers:      map[string]string{"session": "ws://127.0.0.1:9301/spv2"},
		Logger:     log.New(io.Discard, "", 0),
	})
	name, addr, err := s.Resolve("session")
	if err != nil {
		t.Fatal(err)
	}
	if name != "session" || addr != "ws://127.0.0.1:9301/spv2" {
		t.Fatalf("静态对端解析不符: %s %s", name, addr)
	}
	if _, _, err := s.Resolve("missing"); err == nil {
		t.Fatal("未知目标必须报错")
	}
}
