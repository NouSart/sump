package server

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func nativeServerURL(s *Server) string {
	// 骨架的 WsURL 已经以 /spv2 结尾，不再需要拼路径。
	return s.WsURL()
}

func dialNativeMCP(t *testing.T, s *Server) *spv2.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, nativeServerURL(s))
	if err != nil {
		t.Fatalf("连接 /spv2 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// /spv2 的 import method 直接解 Frame.Body，卡片声明来自动态 provides。
func TestNativeImportMethodAndCall(t *testing.T) {
	remote := importFixture(t, lookupTool(), func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &arg)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "native:" + arg.Key}}, StructuredContent: map[string]any{"value": "native:" + arg.Key}}, nil
	})
	policy, _ := json.Marshal(exportPolicy{Exports: []string{}, Remotes: []RemoteConfig{{ID: "demo", URL: remote.URL}}, Imports: []ImportConfig{lookupImport()}})
	s := startGateway(t, Config{}, nil, string(policy))
	c := dialNativeMCP(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	card, err := c.Hello(ctx, spv2.ServiceRef("probe"), spv2.ServiceRef("mcp"))
	if err != nil {
		t.Fatal(err)
	}
	method, ok := card.Method("remote_lookup")
	if !ok {
		t.Fatalf("原生卡片缺少 import method: %+v", card.Methods)
	}
	if !method.Tool || method.LongRunning || method.Input["type"] != "object" || method.Output["type"] != "object" {
		t.Fatalf("import method 未映射 provide: %+v", method)
	}
	if _, ok := method.Input["properties"].(map[string]any)["key"]; !ok {
		t.Fatalf("输入 schema 未沿用审查过的契约: %+v", method.Input)
	}

	call, err := spv2.NewCall("work-native", spv2.ServiceRef("probe"), spv2.ServiceRef("mcp"), "remote_lookup", map[string]any{"key": "x"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := c.CallDecode(ctx, call, &out); err != nil {
		t.Fatalf("原生 import 调用失败: %v", err)
	}
	if out["phase"] != "completed" || out["structured_content"].(map[string]any)["value"] != "native:x" {
		t.Fatalf("原生 import 结果错误: %#v", out)
	}

	bad, _ := spv2.NewCall("work-native-bad", spv2.ServiceRef("probe"), spv2.ServiceRef("mcp"), "remote_lookup", map[string]any{"key": ""})
	if err := c.CallDecode(ctx, bad, &out); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非法输入错误码 = %v, want %q", err, spv2.CodeInvalidArgument)
	}
}

// report_result / cancel_import 同样走原生 method，不合成 v1 信封。
func TestNativeReportAndCancelImport(t *testing.T) {
	s := startGateway(t, Config{}, nil, `{}`)
	c := dialNativeMCP(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()

	trace := protocol.NewID()
	jobID, err := s.jobs.Reserve("owner", "timer", "timer__sleep", trace, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.jobs.BindReceipt(jobID, "item-1"); err != nil {
		t.Fatal(err)
	}
	report, _ := spv2.NewCall(trace, spv2.ServiceRef("timer"), spv2.ServiceRef("mcp"), "report_result",
		protocol.AsyncResultPayload{ItemID: "item-1", Phase: "done", Text: "ok", Final: true})
	var reportOut map[string]any
	if err := c.CallDecode(ctx, report, &reportOut); err != nil {
		t.Fatalf("原生 report_result 失败: %v", err)
	}
	if reportOut["accepted"] != true {
		t.Fatalf("原生 report_result 未受理: %#v", reportOut)
	}
	snapshot, err := s.jobs.Read("owner", jobID)
	if err != nil || !snapshot.Final || snapshot.Phase != "done" {
		t.Fatalf("原生补报未落到观察: %+v %v", snapshot, err)
	}

	cancelCall, _ := spv2.NewCall("work-cancel", spv2.ServiceRef("reasoner"), spv2.ServiceRef("mcp"), "cancel_import", map[string]any{"item_id": "missing"})
	var cancelOut map[string]any
	if err := c.CallDecode(ctx, cancelCall, &cancelOut); err != nil {
		t.Fatalf("原生 cancel_import 失败: %v", err)
	}
	if cancelOut["status"] != "unknown" {
		t.Fatalf("原生 cancel_import 结果错误: %#v", cancelOut)
	}
}

// 原生异步 import 只回受理回执，最终结果仍按 trace 发回原调用方。
func TestNativeAsyncImportReceiptAndReport(t *testing.T) {
	reports := make(chan receivedImportReport, 1)
	caller := newNativePeer(t, "reasoner", []string{"report_result"}, func(call *spv2.Call) (any, error) {
		if call.Frame.Method != "report_result" {
			return nil, errors.New("expected report_result")
		}
		var result protocol.AsyncResultPayload
		if err := call.Frame.DecodeBody(&result); err != nil {
			return nil, err
		}
		reports <- receivedImportReport{frame: call.Frame, result: result}
		return map[string]any{"received": true}, nil
	})
	remote := importFixture(t, lookupTool(), func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &arg)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "async:" + arg.Key}}, nil
	})
	config := lookupImport()
	config.Async = true
	policy, _ := json.Marshal(exportPolicy{Exports: []string{}, Remotes: []RemoteConfig{{ID: "demo", URL: remote.URL}}, Imports: []ImportConfig{config}})
	s := startGateway(t, Config{AsyncTimeout: 5 * time.Second}, []protocol.ServiceCard{{Name: "reasoner", Addr: caller.URL()}}, string(policy))
	c := dialNativeMCP(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	call, _ := spv2.NewCall("work-async-native", spv2.ServiceRef("reasoner"), spv2.ServiceRef("mcp"), "remote_lookup", map[string]any{"key": "x"})
	var receipt importReceipt
	if err := c.CallDecode(ctx, call, &receipt); err != nil {
		t.Fatalf("原生异步 import 受理失败: %v", err)
	}
	if receipt.ItemID == "" || receipt.Phase != "accepted" {
		t.Fatalf("原生异步回执错误: %+v", receipt)
	}
	select {
	case report := <-reports:
		// 原生帧的来源即本跳的 boss：mcp 把结果按原 trace 送回调用方。
		if report.frame.From.ID != "mcp" || report.frame.To.ID != "reasoner" || report.frame.Work != "work-async-native" {
			t.Fatalf("原生异步补报丢失关联: %+v", report.frame)
		}
		if report.result.ItemID != receipt.ItemID || report.result.Phase != "completed" || !strings.Contains(report.result.Text, "async:x") {
			t.Fatalf("原生异步补报错误: %+v", report.result)
		}
	case <-ctx.Done():
		t.Fatal("没有收到原生异步补报")
	}
}
