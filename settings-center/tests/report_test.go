package tests

// 错误上报测试：服务（WS 客户端）→ 中心 report 动作：
// 回执 OK、消费挂点（OnReport）收到来源与内容；message 必填。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/settings-center/server"
)

// TestReportAction 验证 report 的受理、挂点内容与必填校验。
func TestReportAction(t *testing.T) {
	var mu sync.Mutex
	var froms []string
	var got []protocol.ReportPayload

	s, err := server.Start(server.Config{
		Addr: "127.0.0.1:0",
		OnReport: func(from string, r protocol.ReportPayload) {
			mu.Lock()
			froms = append(froms, from)
			got = append(got, r)
			mu.Unlock()
		},
	}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })

	c, err := protocol.Dial("ws://"+s.Addr()+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 正常上报：回执 OK，挂点已收到来源与内容（挂点在回执前同步调用）。
	raw, err := protocol.EncodePayload(protocol.ReportPayload{Kind: "fire", Message: "跳转 memory(store) 未送达"})
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "svc-x", "settings-center", "", protocol.JumpPayload{Action: "report", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("report 未被受理: %s", rp.Error)
	}

	mu.Lock()
	if len(got) != 1 || froms[0] != "svc-x" || got[0].Kind != "fire" || got[0].Message != "跳转 memory(store) 未送达" {
		mu.Unlock()
		t.Fatalf("挂点内容不符: from=%v got=%+v", froms, got)
	}
	mu.Unlock()

	// message 必填：空上报应被拒绝。
	raw2, err := protocol.EncodePayload(protocol.ReportPayload{Kind: "x"})
	if err != nil {
		t.Fatal(err)
	}
	env2, err := protocol.NewEnvelope(protocol.TypeJump, "svc-x", "settings-center", "", protocol.JumpPayload{Action: "report", Input: raw2})
	if err != nil {
		t.Fatal(err)
	}
	resp2, err := c.Call(ctx, env2)
	if err != nil {
		t.Fatal(err)
	}
	var rp2 protocol.ResponsePayload
	if err := resp2.DecodePayload(&rp2); err != nil {
		t.Fatal(err)
	}
	if rp2.OK {
		t.Fatal("空 message 的上报应被拒绝")
	}
}
