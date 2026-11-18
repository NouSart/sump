package tests

// 门面启动硬门：内部服务缺失时不得带病对外。

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
)

func TestFacadeStartFailsWhenPeerMissing(t *testing.T) {
	s := server.New(server.Config{
		Name:   "memory",
		Role:   server.RoleFacade,
		Listen: "127.0.0.1:0",
		Center: "ws://127.0.0.1:1/spv2",
		Peers:  map[string]string{},
	}, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := s.Start(ctx)
	if err == nil {
		t.Fatal("缺内部服务时门面不得启动")
	}
	if !strings.Contains(err.Error(), "session") {
		t.Fatalf("错误应指明缺失的对端: %v", err)
	}
}
