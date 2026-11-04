package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type receivedImportReport struct {
	env    protocol.Envelope
	result protocol.AsyncResultPayload
}

func asyncImportGateway(t *testing.T, endpoint string, cfg Config) (*Server, <-chan receivedImportReport) {
	t.Helper()
	reports := make(chan receivedImportReport, 16)
	caller := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		var jump protocol.JumpPayload
		if err := env.DecodePayload(&jump); err != nil || jump.Action != "report_result" {
			return nil, errors.New("expected report_result")
		}
		var result protocol.AsyncResultPayload
		if err := protocol.DecodeRaw(jump.Input, &result); err != nil {
			return nil, err
		}
		reports <- receivedImportReport{env: env, result: result}
		return map[string]any{"received": true}, nil
	})
	c := lookupImport()
	c.Async = true
	policy, _ := json.Marshal(exportPolicy{Exports: []string{}, Remotes: []RemoteConfig{{ID: "demo", URL: endpoint}}, Imports: []ImportConfig{c}})
	s := startGateway(t, cfg, []protocol.ServiceCard{{Name: "reasoner", Addr: caller}}, string(policy))
	return s, reports
}

func submitAsyncImport(s *Server, ctx context.Context, key any) (importReceipt, error) {
	raw, _ := protocol.EncodePayload(map[string]any{"key": key})
	result, err := s.handleImport(ctx, protocol.Envelope{From: "reasoner", To: "mcp", Trace: "shared-trace", Boss: "portal"}, protocol.JumpPayload{Action: "remote_lookup", Input: raw})
	if err != nil {
		return importReceipt{}, err
	}
	receipt, ok := result.(importReceipt)
	if !ok {
		return importReceipt{}, errors.New("async action did not return an import receipt")
	}
	return receipt, nil
}

func receiveImportReport(t *testing.T, reports <-chan receivedImportReport) receivedImportReport {
	t.Helper()
	select {
	case report := <-reports:
		if report.env.From != "mcp" || report.env.To != "reasoner" || report.env.Trace != "shared-trace" || report.env.Boss != "portal" {
			t.Fatalf("report lost routing association: %+v", report.env)
		}
		if !report.result.Final || report.result.Text == "" {
			t.Fatalf("report lacks readable final result: %+v", report.result)
		}
		return report
	case <-time.After(5 * time.Second):
		t.Fatal("no final report")
		return receivedImportReport{}
	}
}

func TestAsyncImportsAcceptedLifetimeAndParallelResults(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	remote := importFixture(t, lookupTool(), func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var arg struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &arg)
		if arg.Key == "slow" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "result:" + arg.Key}}, StructuredContent: map[string]any{"value": arg.Key}}, nil
	})
	s, reports := asyncImportGateway(t, remote.URL, Config{AsyncTimeout: 5 * time.Second})
	if p := s.imports.Provides(); len(p) != 1 || !p[0].Async || p[0].Batchable {
		t.Fatalf("async contract not advertised: %+v", p)
	}
	requestCtx, done := context.WithCancel(t.Context())
	slow, err := submitAsyncImport(s, requestCtx, "slow")
	done() // models the service skeleton canceling the receipt handler context
	if err != nil || slow.ItemID == "" || slow.Phase != "accepted" {
		t.Fatalf("receipt: %+v %v", slow, err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("worker did not outlive receipt context")
	}
	select {
	case r := <-reports:
		t.Fatalf("blocked tool reported completion: %+v", r)
	default:
	}
	fast, err := submitAsyncImport(s, t.Context(), "fast")
	if err != nil || fast.ItemID == slow.ItemID {
		t.Fatalf("parallel receipt: %+v %v", fast, err)
	}
	first := receiveImportReport(t, reports)
	if first.result.ItemID != fast.ItemID || first.result.Phase != "completed" || !strings.Contains(first.result.Text, "result:fast") {
		t.Fatalf("fast result waited or crossed calls: %+v", first.result)
	}
	once.Do(func() { close(release) })
	last := receiveImportReport(t, reports)
	if last.result.ItemID != slow.ItemID || !strings.Contains(last.result.Text, "result:slow") {
		t.Fatalf("slow result crossed calls: %+v", last.result)
	}
	var data map[string]any
	if err := protocol.DecodeRaw(last.result.Data, &data); err != nil || data["structured_content"].(map[string]any)["value"] != "slow" {
		t.Fatalf("structured data lost: %#v %v", data, err)
	}
}

func TestAsyncImportsRejectBeforeReceiptAndKeepCapacity(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	remote := importFixture(t, lookupTool(), func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
		}
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "ok"}}, nil
	})
	s, reports := asyncImportGateway(t, remote.URL, Config{MaxCalls: 1, MaxCallsPerPrincipal: 1, AsyncTimeout: 5 * time.Second})
	if _, err := submitAsyncImport(s, t.Context(), 123); err == nil {
		t.Fatal("invalid input accepted")
	}
	first, err := submitAsyncImport(s, t.Context(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool not entered")
	}
	if _, err := submitAsyncImport(s, t.Context(), "excess"); err == nil {
		t.Fatal("receipt freed running call capacity")
	}
	if calls.Load() != 1 {
		t.Fatal("rejected calls reached remote")
	}
	once.Do(func() { close(release) })
	if r := receiveImportReport(t, reports); r.result.ItemID != first.ItemID {
		t.Fatal("wrong final")
	}
	drain, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := s.asyncImports.Close(drain); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	remaining := s.calls
	s.mu.Unlock()
	if remaining != 0 {
		t.Fatalf("call quota leaked: %d", remaining)
	}
}

func TestAsyncImportsCancellationIsOwnerScopedAndNotRemoteStop(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	var calls atomic.Int32
	remote := importFixture(t, lookupTool(), func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		close(entered)
		<-release // intentionally ignores transport cancellation
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "done"}}, nil
	})
	s, reports := asyncImportGateway(t, remote.URL, Config{AsyncTimeout: 5 * time.Second})
	receipt, err := submitAsyncImport(s, t.Context(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool not entered")
	}
	raw, _ := protocol.EncodePayload(map[string]any{"item_id": receipt.ItemID})
	cancelCall := func(from string) map[string]any {
		t.Helper()
		out, err := s.handleCancelImport(t.Context(), protocol.Envelope{From: from}, protocol.JumpPayload{Input: raw})
		if err != nil {
			t.Fatal(err)
		}
		return out.(map[string]any)
	}
	if got := cancelCall("other"); got["status"] != "unknown" {
		t.Fatalf("cross-owner cancellation: %#v", got)
	}
	select {
	case r := <-reports:
		t.Fatalf("other caller canceled work: %+v", r)
	default:
	}
	if got := cancelCall("reasoner"); got["status"] != "observation_cancelled" || got["remote_outcome"] != "unknown" {
		t.Fatalf("misleading cancellation: %#v", got)
	}
	final := receiveImportReport(t, reports)
	if final.result.ItemID != receipt.ItemID || final.result.Phase != "outcome_unknown" || calls.Load() != 1 {
		t.Fatalf("cancel replayed or pretended completion: %+v count=%d", final.result, calls.Load())
	}
	if got := cancelCall("reasoner"); got["status"] != "unknown" {
		t.Fatalf("ended observation still cancellable: %#v", got)
	}
	once.Do(func() { close(release) })
}

func TestAsyncImportsShutdownDrainsUnknownResult(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	remote := importFixture(t, lookupTool(), func(_ context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-release
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "done"}}, nil
	})
	s, reports := asyncImportGateway(t, remote.URL, Config{AsyncTimeout: 5 * time.Second, DeliveryTimeout: time.Second})
	receipt, err := submitAsyncImport(s, t.Context(), "slow")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("tool not entered")
	}
	drain, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if err := s.asyncImports.Close(drain); err != nil {
		t.Fatal(err)
	}
	final := receiveImportReport(t, reports)
	if final.result.ItemID != receipt.ItemID || final.result.Phase != "outcome_unknown" {
		t.Fatalf("shutdown invented outcome: %+v", final.result)
	}
	if _, err := submitAsyncImport(s, t.Context(), "after-close"); err == nil {
		t.Fatal("shutdown accepted work")
	}
	once.Do(func() { close(release) })
}

func TestPreparedImportsReserveBeforeAcceptanceAndDoNotReplay(t *testing.T) {
	var calls atomic.Int32
	remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "ok"}}, nil
	})
	i := newTestImports(t, remote.URL, time.Second, 16384, nil)
	raw, _ := protocol.EncodePayload(map[string]any{"key": "x"})
	input := protocol.JumpPayload{Action: "remote_lookup", Input: raw}
	var prepared []*preparedImport
	for n := 0; n < 4; n++ {
		p, err := i.Prepare(input)
		if err != nil {
			t.Fatal(err)
		}
		prepared = append(prepared, p)
		defer p.Close()
	}
	if _, err := i.Prepare(input); err == nil {
		t.Fatal("remote capacity not reserved before acceptance")
	}
	if calls.Load() != 0 {
		t.Fatal("preparation dispatched remote business")
	}
	if _, err := prepared[0].Run(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err := prepared[0].Run(t.Context()); err == nil {
		t.Fatal("prepared tool replayed")
	}
	prepared[0].Close()
	prepared[0].Close()
	p, err := i.Prepare(input)
	if err != nil {
		t.Fatal("reservation was not released", err)
	}
	p.Close()
	if calls.Load() != 1 {
		t.Fatalf("unexpected calls: %d", calls.Load())
	}
}

func TestImportReportProjectsTextAndPreservesFailureSemantics(t *testing.T) {
	text := strings.Repeat("中文", 5000)
	value := map[string]any{"phase": "failed", "is_error": true, "content": []any{map[string]any{"type": "text", "text": text}}, "structured_content": map[string]any{"value": "original"}}
	r := importReport("id", value, nil, true)
	if r.Phase != "failed" || !strings.Contains(r.Text, "已截断") || len(r.Text) > 8400 {
		t.Fatalf("unbounded or wrong projection: %s bytes=%d", r.Phase, len(r.Text))
	}
	var decoded map[string]any
	if err := protocol.DecodeRaw(r.Data, &decoded); err != nil || decoded["structured_content"].(map[string]any)["value"] != "original" {
		t.Fatal("structured result changed", err)
	}
	before := importReport("id", nil, errors.New("unavailable"), false)
	after := importReport("id", nil, errors.New("unavailable"), true)
	if before.Phase != "failed" || after.Phase != "outcome_unknown" {
		t.Fatalf("dispatch evidence lost: %s %s", before.Phase, after.Phase)
	}
}

func TestImportDeliveryAcknowledgments(t *testing.T) {
	for _, value := range []any{nil, map[string]any{}, map[string]any{"received": true}, map[string]any{"status": "accepted"}, map[string]any{"status": "buffered"}, map[string]any{"status": "duplicate"}} {
		raw, _ := protocol.EncodePayload(value)
		if importDeliveryUnconfirmed(raw) {
			t.Fatalf("existing successful acknowledgment rejected: %#v", value)
		}
	}
	for _, value := range []any{map[string]any{"status": "ignored"}, map[string]any{"accepted": false}, map[string]any{"received": false}} {
		raw, _ := protocol.EncodePayload(value)
		if !importDeliveryUnconfirmed(raw) {
			t.Fatalf("negative acknowledgment treated as delivered: %#v", value)
		}
	}
	if !importDeliveryUnconfirmed([]byte{0xdf, 0x7f, 0xff, 0xff, 0xff}) {
		t.Fatal("malformed acknowledgment treated as delivered")
	}
}

func TestAsyncImportsDeliveryFailureReportsWithoutReplayingBusiness(t *testing.T) {
	for _, mode := range []string{"ignored", "accepted_false", "received_false", "response_error"} {
		t.Run(mode, func(t *testing.T) {
			var executions, deliveries atomic.Int32
			remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				executions.Add(1)
				return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "executed"}}, nil
			})
			caller := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
				var jump protocol.JumpPayload
				if err := env.DecodePayload(&jump); err != nil || jump.Action != "report_result" {
					return nil, errors.New("expected final report")
				}
				deliveries.Add(1)
				switch mode {
				case "ignored":
					return map[string]any{"status": "ignored"}, nil
				case "accepted_false":
					return map[string]any{"accepted": false}, nil
				case "received_false":
					return map[string]any{"received": false}, nil
				default:
					return nil, errors.New("receiver temporarily unavailable")
				}
			})
			failures := make(chan protocol.ReportPayload, 4)
			center := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
				if env.Type == protocol.TypeRegister {
					return protocol.RosterPayload{Services: []protocol.ServiceCard{{Name: "reasoner", Addr: caller}}, Revision: 1}, nil
				}
				var jump protocol.JumpPayload
				if env.Type == protocol.TypeJump && env.DecodePayload(&jump) == nil && jump.Action == "report" {
					var report protocol.ReportPayload
					if err := protocol.DecodeRaw(jump.Input, &report); err != nil {
						return nil, err
					}
					failures <- report
				}
				return protocol.SettingsResult{}, nil
			})
			dir := t.TempDir()
			c := lookupImport()
			c.Async = true
			policy, _ := json.Marshal(exportPolicy{Exports: []string{}, Remotes: []RemoteConfig{{ID: "demo", URL: remote.URL}}, Imports: []ImportConfig{c}})
			policyPath := filepath.Join(dir, "mcp.json")
			if err := os.WriteFile(policyPath, policy, 0600); err != nil {
				t.Fatal(err)
			}
			s, err := New(Config{Listen: "127.0.0.1:0", Center: center, HTTP: HTTPOptions{Listen: "127.0.0.1:0"}, ConfigPath: policyPath, SecretFile: filepath.Join(dir, "secrets.json"), Logger: log.New(io.Discard, "", 0), DeliveryTimeout: time.Second})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.Start(t.Context()); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			receipt, err := submitAsyncImport(s, t.Context(), "x")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case report := <-failures:
				if report.Kind != "fire" || !strings.Contains(report.Message, receipt.ItemID) || !strings.Contains(report.Message, "phase=completed") || !strings.Contains(report.Message, "delivery unconfirmed") {
					t.Fatalf("execution and delivery errors conflated: %+v", report)
				}
			case <-time.After(3 * time.Second):
				t.Fatal("rejected delivery was not reported to the settings center")
			}
			drain, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			if err := s.asyncImports.Close(drain); err != nil {
				t.Fatal(err)
			}
			if executions.Load() != 1 || deliveries.Load() != 1 {
				t.Fatalf("replayed business or acknowledged delivery: executions=%d deliveries=%d", executions.Load(), deliveries.Load())
			}
		})
	}
}
