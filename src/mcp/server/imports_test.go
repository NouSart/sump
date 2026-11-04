package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const lookupInput = `{"type":"object","properties":{"key":{"type":"string","minLength":1,"maxLength":128}},"required":["key"],"additionalProperties":false}`
const lookupOutput = `{"type":"object","properties":{"value":{"type":"string","maxLength":4096}},"required":["value"],"additionalProperties":false}`

func lookupImport() ImportConfig {
	return ImportConfig{Action: "remote_lookup", Remote: "demo", Tool: "lookup", InputSchema: json.RawMessage(lookupInput), OutputSchema: json.RawMessage(lookupOutput)}
}

func lookupTool() *mcp.Tool {
	var input, output any
	_ = json.Unmarshal([]byte(lookupInput), &input)
	_ = json.Unmarshal([]byte(lookupOutput), &output)
	return &mcp.Tool{Name: "lookup", InputSchema: input, OutputSchema: output}
}

func importFixture(t *testing.T, tool *mcp.Tool, fn func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error)) *httptest.Server {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "independent-lookup", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{importProtocolVersion}})
	server.AddTool(tool, fn)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true})
	s := httptest.NewServer(h)
	t.Cleanup(s.Close)
	return s
}

func newTestImports(t *testing.T, endpoint string, timeout time.Duration, maxBytes int, token func(string) (string, uint64)) *Imports {
	t.Helper()
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: endpoint}}, []ImportConfig{lookupImport()}, timeout, maxBytes, token)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(i.Close)
	return i
}

func callImport(i *Imports, ctx context.Context, key any) (any, error) {
	raw, _ := protocol.EncodePayload(map[string]any{"key": key})
	return i.Handle(ctx, protocol.Envelope{From: "reasoner", To: "mcp", Trace: "same-trace"}, protocol.JumpPayload{Action: "remote_lookup", Input: raw})
}

func TestImportsSuccessfulTranslation(t *testing.T) {
	var calls atomic.Int32
	remote := importFixture(t, lookupTool(), func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		var arg struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal(req.Params.Arguments, &arg); err != nil {
			t.Error(err)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "external:" + arg.Key}}, StructuredContent: map[string]any{"value": "external:" + arg.Key}}, nil
	})
	i := newTestImports(t, remote.URL, 2*time.Second, 16384, nil)
	provides := i.Provides()
	if len(provides) != 1 || provides[0].Action != "remote_lookup" || !provides[0].Tool || provides[0].Async || provides[0].Batchable {
		t.Fatalf("wrong provides: %+v", provides)
	}
	got, err := callImport(i, context.Background(), "你好")
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	if out["phase"] != "completed" || out["is_error"] != false || out["structured_content"].(map[string]any)["value"] != "external:你好" {
		t.Fatalf("wrong output: %#v", got)
	}
	content := out["content"].([]any)[0].(map[string]any)
	if content["type"] != "text" || content["text"] != "external:你好" || calls.Load() != 1 {
		t.Fatalf("content/count mismatch: %#v / %d", content, calls.Load())
	}
}

func TestImportsRejectInvalidInputBeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer remote.Close()
	i := newTestImports(t, remote.URL, time.Second, 16384, nil)
	for _, value := range []any{true, 12, "", strings.Repeat("x", 129)} {
		if _, err := callImport(i, context.Background(), value); err == nil || !strings.Contains(err.Error(), "not submitted") {
			t.Fatalf("input %#v: %v", value, err)
		}
	}
	raw, _ := protocol.EncodePayload(map[string]any{"key": "ok", "remote": "other"})
	if _, err := i.Handle(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Action: "remote_lookup", Input: raw}); err == nil {
		t.Fatal("accepted extra routing field")
	}
	if _, err := i.Handle(context.Background(), protocol.Envelope{}, protocol.JumpPayload{Action: "unmapped", Input: raw}); err == nil {
		t.Fatal("accepted unreviewed action")
	}
	if requests.Load() != 0 {
		t.Fatalf("rejected input reached network: %d", requests.Load())
	}
}

func TestImportsRejectOversizedMessagePackHeadersBeforeAllocation(t *testing.T) {
	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer remote.Close()
	i := newTestImports(t, remote.URL, time.Second, 16384, nil)
	// Only the header is present; an unbounded generic decoder could allocate
	// from the declared length before discovering the missing payload.
	for _, raw := range [][]byte{
		{0xdf, 0x7f, 0xff, 0xff, 0xff}, // map32
		{0xdd, 0x7f, 0xff, 0xff, 0xff}, // array32
		{0xdb, 0x7f, 0xff, 0xff, 0xff}, // str32
		{0x81, 0xa3, 'k', 'e', 'y', 0xdd, 0x7f, 0xff, 0xff, 0xff},
		{0x81, 0xa3, 'k', 'e', 'y', 0xc6, 0x7f, 0xff, 0xff, 0xff},
	} {
		if _, err := i.Prepare(protocol.JumpPayload{Action: "remote_lookup", Input: raw}); err == nil || !strings.Contains(err.Error(), "MCP_INVALID_INPUT") {
			t.Fatalf("malformed length accepted: %x %v", raw, err)
		}
	}
	if requests.Load() != 0 {
		t.Fatal("malformed input reached the remote")
	}
}

func TestImportsSchemaDriftStopsCall(t *testing.T) {
	var calls atomic.Int32
	tool := lookupTool()
	tool.InputSchema = map[string]any{"type": "object"}
	remote := importFixture(t, tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	i := newTestImports(t, remote.URL, 2*time.Second, 16384, nil)
	if _, err := callImport(i, context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "MCP_CONTRACT_MISMATCH") {
		t.Fatalf("schema drift: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("changed contract executed")
	}
}

func TestImportsToolFailureIsData(t *testing.T) {
	remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "unknown key"}}}, nil
	})
	i := newTestImports(t, remote.URL, 2*time.Second, 16384, nil)
	got, err := callImport(i, context.Background(), "missing")
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	if out["is_error"] != true || out["phase"] != "failed" {
		t.Fatalf("tool failure lost: %#v", got)
	}
}

func TestImportsTimeoutDoesNotReplayAndRecovers(t *testing.T) {
	var calls atomic.Int32
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	defer close(release)
	remote := importFixture(t, lookupTool(), func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		if strings.Contains(string(req.Params.Arguments), "slow") {
			entered <- struct{}{}
			// A remote may keep executing after HTTP cancellation. The bridge
			// must still return unknown, never retry or claim it was stopped.
			<-release
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "ok"}}, nil
	})
	i := newTestImports(t, remote.URL, 250*time.Millisecond, 16384, nil)
	_, err := callImport(i, context.Background(), "slow")
	if err == nil || !strings.Contains(err.Error(), "MCP_OUTCOME_UNKNOWN") {
		t.Fatalf("timeout semantics: %v", err)
	}
	select {
	case <-entered:
	default:
		t.Fatal("tool did not actually start")
	}
	if calls.Load() != 1 {
		t.Fatalf("timed-out tool replayed: %d", calls.Load())
	}
	if _, err := callImport(i, context.Background(), "next"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("new invocation count: %d", calls.Load())
	}
}

func TestImportsRemoteFailureIsolation(t *testing.T) {
	broken := httptest.NewServer(http.NotFoundHandler())
	defer broken.Close()
	var calls atomic.Int32
	healthy := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "healthy"}}, nil
	})
	a, b := lookupImport(), lookupImport()
	a.Remote, a.Action = "broken", "broken_lookup"
	i, err := NewImports([]RemoteConfig{{ID: "broken", URL: broken.URL}, {ID: "demo", URL: healthy.URL}}, []ImportConfig{a, b}, time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	raw, _ := protocol.EncodePayload(map[string]any{"key": "x"})
	if _, err = i.Handle(t.Context(), protocol.Envelope{}, protocol.JumpPayload{Action: a.Action, Input: raw}); err == nil {
		t.Fatal("unavailable target succeeded")
	}
	if _, err = callImport(i, t.Context(), "x"); err != nil || calls.Load() != 1 {
		t.Fatalf("failure leaked across remotes: %v", err)
	}
}

func TestImportsRejectAmbiguousConfigAndNumbers(t *testing.T) {
	for _, raw := range []string{
		`{"id":"x","URL":"http://127.0.0.1:1"}`,
		`{"id":"x","url":"http://127.0.0.1:1","token_required":null}`,
		`{"id":"x","id":"y","url":"http://127.0.0.1:1"}`,
	} {
		var remote RemoteConfig
		if json.Unmarshal([]byte(raw), &remote) == nil {
			t.Fatalf("ambiguous remote config accepted: %s", raw)
		}
	}
	for _, value := range []any{int64(9007199254740992), uint64(9007199254740993), float64(9007199254740992)} {
		if checkImportValue(value, 0) == nil {
			t.Fatalf("unsafe integer accepted: %v", value)
		}
	}
}

func TestImportsConcurrentCallsRemainIndependent(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	remote := importFixture(t, lookupTool(), func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var in struct {
			Key string `json:"key"`
		}
		_ = json.Unmarshal(req.Params.Arguments, &in)
		if in.Key == "slow" {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": in.Key}}, nil
	})
	i := newTestImports(t, remote.URL, 2*time.Second, 16384, nil)
	type answer struct {
		out any
		err error
	}
	slow := make(chan answer, 1)
	go func() { out, err := callImport(i, context.Background(), "slow"); slow <- answer{out, err} }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("slow call did not reach remote")
	}
	fast, err := callImport(i, context.Background(), "fast")
	if err != nil || fast.(map[string]any)["structured_content"].(map[string]any)["value"] != "fast" {
		t.Fatalf("fast call: %#v %v", fast, err)
	}
	select {
	case <-slow:
		t.Fatal("slow completed before its release")
	default:
	}
	close(release)
	result := <-slow
	if result.err != nil || result.out.(map[string]any)["structured_content"].(map[string]any)["value"] != "slow" {
		t.Fatalf("slow call: %#v %v", result.out, result.err)
	}
}

func TestImportsCredentialsAndRedirects(t *testing.T) {
	var forwarded atomic.Int32
	unexpected := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded.Add(1) }))
	defer unexpected.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, unexpected.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	i := newTestImports(t, redirect.URL, time.Second, 16384, func(string) (string, uint64) { return "private-remote-token", 0 })
	if _, err := callImport(i, context.Background(), "x"); err == nil || strings.Contains(err.Error(), "private-remote-token") {
		t.Fatalf("redirect error: %v", err)
	}
	if forwarded.Load() != 0 {
		t.Fatal("followed a redirect with credentials")
	}

	var requests atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer remote.Close()
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: remote.URL, TokenRequired: true}}, []ImportConfig{lookupImport()}, time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if _, err := callImport(i, context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "MCP_AUTH_REQUIRED") {
		t.Fatalf("missing token: %v", err)
	}
	if requests.Load() != 0 {
		t.Fatal("missing required token reached remote")
	}
}

func TestImportsConfigurationValidation(t *testing.T) {
	valid := RemoteConfig{ID: "demo", URL: "http://127.0.0.1:1234/mcp"}
	for _, endpoint := range []string{"http://example.com/mcp", "file:///tmp/mcp", "https://user:secret@example.com/mcp", "https://example.com/mcp?token=secret", "https://example.com/mcp#frag"} {
		r := valid
		r.URL = endpoint
		if ValidateImportConfigs([]RemoteConfig{r}, nil) == nil {
			t.Errorf("accepted endpoint %q", endpoint)
		}
	}
	for _, action := range []string{"configure", "report_result", "ping", "set_setting", "Invalid", "a__tool"} {
		c := lookupImport()
		c.Action = action
		// Double underscores are valid action identifiers; the prefix split remains unambiguous.
		if action == "a__tool" {
			continue
		}
		if ValidateImportConfigs([]RemoteConfig{valid}, []ImportConfig{c}) == nil {
			t.Errorf("accepted action %q", action)
		}
	}
	if ValidateImportConfigs([]RemoteConfig{valid, valid}, nil) == nil {
		t.Fatal("accepted duplicate remote")
	}
	if ValidateImportConfigs([]RemoteConfig{valid}, []ImportConfig{lookupImport(), lookupImport()}) == nil {
		t.Fatal("accepted duplicate action")
	}
	c := lookupImport()
	c.InputSchema = json.RawMessage(`{"type":"object","$ref":"https://example.com/schema.json"}`)
	if ValidateImportConfigs([]RemoteConfig{valid}, []ImportConfig{c}) == nil {
		t.Fatal("accepted external schema reference")
	}
	if ValidateRemoteToken("secret\r\nheader") == nil {
		t.Fatal("accepted header injection")
	}
}

func TestImportTransportBoundsReadAndClose(t *testing.T) {
	body := &countingImportBody{remaining: 100000}
	reader := &importBody{ReadCloser: body, remaining: 4096}
	_, err := io.ReadAll(reader)
	if err == nil || body.read > 4097 {
		t.Fatalf("unbounded response: %d %v", body.read, err)
	}
	_ = reader.Close()
	if !body.closed {
		t.Fatal("body not closed")
	}
}

type countingImportBody struct {
	remaining, read int
	closed          bool
}

func (r *countingImportBody) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	for idx := range n {
		p[idx] = 'x'
	}
	r.remaining -= n
	r.read += n
	return n, nil
}
func (r *countingImportBody) Close() error { r.closed = true; return nil }

func TestImportsCloseCancelsCalls(t *testing.T) {
	entered := make(chan struct{})
	remote := importFixture(t, lookupTool(), func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	i := newTestImports(t, remote.URL, 5*time.Second, 16384, nil)
	var wg sync.WaitGroup
	wg.Add(1)
	done := make(chan error, 1)
	go func() { defer wg.Done(); _, err := callImport(i, context.Background(), "x"); done <- err }()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("remote did not start")
	}
	i.Close()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("closed call succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("close did not release call")
	}
	wg.Wait()
	if _, err := callImport(i, context.Background(), "x"); err == nil || !strings.Contains(err.Error(), "not submitted") {
		t.Fatalf("closed importer: %v", err)
	}
}

func TestImportsStructuredTypesRoundTrip(t *testing.T) {
	const shape = `{"type":"object"}`
	var schema any
	_ = json.Unmarshal([]byte(shape), &schema)
	var calls atomic.Int32
	remote := importFixture(t, &mcp.Tool{Name: "echo_data", InputSchema: schema, OutputSchema: schema}, func(_ context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		var value any
		if err := json.Unmarshal(req.Params.Arguments, &value); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{StructuredContent: value}, nil
	})
	cfg := ImportConfig{Action: "echo_data", Remote: "demo", Tool: "echo_data", InputSchema: json.RawMessage(shape), OutputSchema: json.RawMessage(shape)}
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: remote.URL}}, []ImportConfig{cfg}, time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	payload := map[string]any{"text": "你好🙂", "integer": int64(9007199254740991), "decimal": 1.25, "null": nil, "empty": map[string]any{}, "nested": []any{true, "x", nil}}
	raw, _ := protocol.EncodePayload(payload)
	value, err := i.Handle(t.Context(), protocol.Envelope{}, protocol.JumpPayload{Action: "echo_data", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	got := value.(map[string]any)["structured_content"].(map[string]any)
	wantJSON, _ := json.Marshal(payload)
	gotJSON, _ := json.Marshal(got)
	if string(wantJSON) != string(gotJSON) {
		t.Fatalf("value changed: want %s got %s", wantJSON, gotJSON)
	}
	payload["integer"] = int64(9007199254740993)
	raw, _ = protocol.EncodePayload(payload)
	if _, err = i.Handle(t.Context(), protocol.Envelope{}, protocol.JumpPayload{Action: "echo_data", Input: raw}); err == nil || calls.Load() != 1 {
		t.Fatal("unsafe integer reached target")
	}
}

func TestImportsRejectInvalidStructuredResult(t *testing.T) {
	remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{StructuredContent: map[string]any{"unexpected": true}}, nil
	})
	i := newTestImports(t, remote.URL, time.Second, 16384, nil)
	if _, err := callImport(i, t.Context(), "x"); err == nil {
		t.Fatal("invalid result reported as success")
	}
}

func TestImportsTextOnlyContract(t *testing.T) {
	var calls atomic.Int32
	tool := lookupTool()
	tool.OutputSchema = nil
	remote := importFixture(t, tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "plain text result"}}}, nil
	})
	cfg := lookupImport()
	cfg.OutputSchema = nil
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: remote.URL}}, []ImportConfig{cfg}, time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	checks := i.Check(t.Context())
	if len(checks) != 1 || !checks[0].OK || calls.Load() != 0 {
		t.Fatalf("preflight executed or rejected text-only tool: %+v", checks)
	}
	got, err := callImport(i, t.Context(), "text")
	if err != nil {
		t.Fatal(err)
	}
	out := got.(map[string]any)
	if out["phase"] != "completed" || out["structured_content"] != nil || out["content"].([]any)[0].(map[string]any)["text"] != "plain text result" || calls.Load() != 1 {
		t.Fatalf("text-only result changed: %#v", got)
	}
}

func TestImportsOmittedOutputSchemaMustMatchRemote(t *testing.T) {
	for _, remoteDeclares := range []bool{false, true} {
		t.Run(fmt.Sprint(remoteDeclares), func(t *testing.T) {
			var calls atomic.Int32
			tool, cfg := lookupTool(), lookupImport()
			if remoteDeclares {
				cfg.OutputSchema = nil
			} else {
				tool.OutputSchema = nil
			}
			remote := importFixture(t, tool, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				calls.Add(1)
				return &mcp.CallToolResult{}, nil
			})
			i, err := NewImports([]RemoteConfig{{ID: "demo", URL: remote.URL}}, []ImportConfig{cfg}, time.Second, 16384, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer i.Close()
			checks := i.Check(t.Context())
			if checks[0].Code != "MCP_CONTRACT_MISMATCH" {
				t.Fatalf("wrong contract result: %+v", checks)
			}
			if _, err := callImport(i, t.Context(), "x"); err == nil || calls.Load() != 0 {
				t.Fatal("schema declaration drift executed a tool")
			}
		})
	}
}
