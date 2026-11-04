package integration_test

// capabilities 必须真的能从 WS 动作入口调到，而不只是直接调处理器。
// 之前的能力测试直接调 s.capabilities，注册漏了也照样绿——这里补上真实链路。

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/NouSart/sump/approval/analysis"
	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	approval "github.com/NouSart/sump/approval/server"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	center "github.com/NouSart/sump/settings-center/server"
)

func TestCapabilitiesActionOverWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	logger := log.New(io.Discard, "", 0)

	c, err := center.Start(center.Config{Addr: "127.0.0.1:0"}, logger)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Shutdown() })
	centerURL := "ws://" + c.Addr() + "/ws"

	e, err := engine.New(analysis.Bash{}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	a, err := approval.New(approval.Config{
		Name: "approval", Listen: "127.0.0.1:0", Center: centerURL,
		Engine: e, PolicyJSON: policy.DefaultJSON, Logger: logger,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Service.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(a.Shutdown)

	client := service.New(service.Config{Name: "observer", Listen: "127.0.0.1:0", Center: centerURL, Logger: logger})
	if err := client.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Shutdown)

	raw, err := client.Call(ctx, "approval", "capabilities", map[string]any{}, "trace-cap", "observer")
	if err != nil {
		t.Fatalf("capabilities 经动作入口调用失败: %v", err)
	}
	var got struct {
		SchemaVersion int      `msgpack:"schema_version"`
		Dialects      []string `msgpack:"dialects"`
		AutoApprove   string   `msgpack:"auto_approve"`
	}
	if err := protocol.DecodeRaw(raw, &got); err != nil {
		t.Fatal(err)
	}
	if got.SchemaVersion != 1 || got.AutoApprove != "low" {
		t.Fatalf("capabilities 回执不符: %+v", got)
	}
	if len(got.Dialects) != 2 || got.Dialects[0] != "powershell" || got.Dialects[1] != "bash" {
		t.Fatalf("方言声明不符: %+v", got.Dialects)
	}
}
