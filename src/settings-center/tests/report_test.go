package tests

// 错误上报测试：服务 → 中心原生 report 方法：
// 回执 OK、消费挂点（OnReport）收到来源与内容；message 必填。

import (
	"sync"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
	"github.com/NouSart/sump/spv2"
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

	c := dialCenter(t, centerURL(s))

	// 正常上报：回执 OK，挂点已收到来源与内容（挂点在回执前同步调用）。
	var ack map[string]any
	if err := callCenter(t, c, "report", protocol.ReportPayload{Kind: "fire", Message: "跳转 memory(store) 未送达"}, &ack); err != nil {
		t.Fatalf("report 未被受理: %v", err)
	}
	if ack["status"] != "ok" {
		t.Fatalf("report ack = %+v", ack)
	}

	mu.Lock()
	if len(got) != 1 || froms[0] != "test" || got[0].Kind != "fire" || got[0].Message != "跳转 memory(store) 未送达" {
		mu.Unlock()
		t.Fatalf("挂点内容不符: from=%v got=%+v", froms, got)
	}
	mu.Unlock()

	// message 必填：空上报应被拒绝。
	err = callCenter(t, c, "report", protocol.ReportPayload{Kind: "x"}, &ack)
	if err == nil {
		t.Fatal("空 message 的上报应被拒绝")
	}
	if !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("应回 invalid_argument: %v", err)
	}
}
