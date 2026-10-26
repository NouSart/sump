package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func httpTestVerifier(_ context.Context, token string, _ *http.Request) (*auth.TokenInfo, error) {
	switch token {
	case "test-token":
		return &auth.TokenInfo{UserID: "owner"}, nil
	case "other-token":
		return &auth.TokenInfo{UserID: "other"}, nil
	case "expired-token":
		return &auth.TokenInfo{UserID: "owner", Expiration: time.Now().Add(-time.Hour)}, nil
	default:
		return nil, errors.New("sensitive verifier diagnostic")
	}
}

func startHTTPTest(t *testing.T, opts HTTPOptions, configure func(*mcp.Server)) *HTTPServer {
	t.Helper()
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:0"
	}
	h, err := NewHTTPServer(opts, configure, httpTestVerifier)
	if err != nil {
		t.Fatal(err)
	}
	if err = h.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = h.Close() })
	return h
}

// Raw HTTP requests exercise the wire independently from the SDK client.
func httpBody(method string, params map[string]any) string {
	if params == nil {
		params = make(map[string]any)
	}
	params["_meta"] = map[string]any{
		mcp.MetaKeyProtocolVersion:    protocolVersion,
		mcp.MetaKeyClientInfo:         map[string]any{"name": "raw-test", "version": "1"},
		mcp.MetaKeyClientCapabilities: map[string]any{},
	}
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	return string(raw)
}

func newHTTPRequest(ctx context.Context, h *HTTPServer, method, body string) (*http.Request, error) {
	r, err := http.NewRequestWithContext(ctx, method, h.URL(), strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", protocolVersion)
	var request struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if json.Unmarshal([]byte(body), &request) == nil {
		r.Header.Set("Mcp-Method", request.Method)
		if request.Params.Name != "" {
			r.Header.Set("Mcp-Name", request.Params.Name)
		}
	}
	return r, nil
}

func httpRequest(t *testing.T, h *HTTPServer, method, body string, edit func(*http.Request)) (int, http.Header, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	r, err := newHTTPRequest(ctx, h, method, body)
	if err != nil {
		t.Fatal(err)
	}
	if edit != nil {
		edit(r)
	}
	resp, err := http.DefaultClient.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Header.Get("Cache-Control") != "no-store" {
		t.Error("HTTP response must not be cached")
	}
	if resp.Header.Get("Mcp-Session-Id") != "" {
		t.Error("stateless endpoint emitted a session ID")
	}
	return resp.StatusCode, resp.Header, string(b)
}

type httpAuthTransport struct{ token string }

func (a httpAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+a.token)
	return http.DefaultTransport.RoundTrip(r)
}

func TestHTTPRealSDKClient(t *testing.T) {
	h := startHTTPTest(t, HTTPOptions{}, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			if r.Extra.TokenInfo == nil || r.Extra.TokenInfo.UserID != "owner" {
				t.Error("missing authenticated identity")
			}
			if r.Session.ID() != "" {
				t.Error("stateless handler has a persistent session identity")
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(r.Params.Arguments)}}}, nil
		})
	})
	client := mcp.NewClient(&mcp.Implementation{Name: "wire-test", Version: "1"}, &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cs, err := client.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: h.URL(), HTTPClient: &http.Client{Transport: httpAuthTransport{"test-token"}}, DisableStandaloneSSE: true, MaxRetries: -1}, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	if cs.InitializeResult().ProtocolVersion != protocolVersion || cs.ID() != "" {
		t.Fatalf("wrong protocol/session: %+v / %q", cs.InitializeResult(), cs.ID())
	}
	listed, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(listed.Tools) != 1 || listed.Tools[0].Name != "echo" || listed.CacheScope != "private" || listed.TTLMs != 0 {
		t.Fatalf("tools/list: %+v", listed)
	}
	res, err := cs.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "你好"}})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError || len(res.Content) != 1 || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "你好") {
		t.Fatalf("tools/call: %+v", res)
	}
}

func TestHTTPStatelessRequests(t *testing.T) {
	h := startHTTPTest(t, HTTPOptions{}, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "identity", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: r.Extra.TokenInfo.UserID}}}, nil
		})
	})
	// Direct tools/list works without initialize or a prior server/discover.
	for _, method := range []string{"tools/list", "server/discover"} {
		status, _, body := httpRequest(t, h, http.MethodPost, httpBody(method, nil), nil)
		if status != 200 {
			t.Fatalf("%s: %d %s", method, status, body)
		}
		var result struct {
			Result struct {
				CacheScope        string   `json:"cacheScope"`
				TTLMs             int      `json:"ttlMs"`
				SupportedVersions []string `json:"supportedVersions"`
			} `json:"result"`
		}
		if err := json.Unmarshal([]byte(body), &result); err != nil {
			t.Fatal(err)
		}
		if result.Result.CacheScope != "private" || result.Result.TTLMs != 0 {
			t.Fatalf("cache metadata: %s", body)
		}
		if method == "server/discover" && (len(result.Result.SupportedVersions) != 1 || result.Result.SupportedVersions[0] != protocolVersion) {
			t.Fatalf("supported versions: %s", body)
		}
	}
	// A bogus legacy session ID cannot carry identity across requests.
	for _, token := range []string{"test-token", "other-token"} {
		status, _, body := httpRequest(t, h, http.MethodPost, httpBody("tools/call", map[string]any{"name": "identity", "arguments": map[string]any{}}), func(r *http.Request) {
			r.Header.Set("Mcp-Session-Id", "untrusted-old-session")
			r.Header.Set("Authorization", "Bearer "+token)
		})
		want := "owner"
		if token == "other-token" {
			want = "other"
		}
		if status != 200 || !strings.Contains(body, want) {
			t.Fatalf("request identity: %d %s", status, body)
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodDelete, http.MethodOptions} {
		status, header, body := httpRequest(t, h, method, "", nil)
		if status != 405 || header.Get("Allow") != "POST" {
			t.Fatalf("%s: %d %s", method, status, body)
		}
	}
}

func TestHTTPAuthorizationAndOrigin(t *testing.T) {
	h := startHTTPTest(t, HTTPOptions{AllowedOrigins: []string{"https://client.example"}}, nil)
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodDelete, http.MethodOptions} {
		t.Run("unauthorized_"+method, func(t *testing.T) {
			status, _, body := httpRequest(t, h, method, httpBody("tools/list", nil), func(r *http.Request) { r.Header.Del("Authorization") })
			if status != 401 || strings.Contains(body, "sensitive") {
				t.Fatalf("unauthorized: %d %s", status, body)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*http.Request)
		want int
	}{
		{"invalid_token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer bad") }, 401},
		{"expired_token", func(r *http.Request) { r.Header.Set("Authorization", "Bearer expired-token") }, 401},
		{"duplicate_auth", func(r *http.Request) { r.Header.Add("Authorization", "Bearer test-token") }, 401},
		{"foreign_host", func(r *http.Request) { r.Host = "attacker.example" }, 403},
		{"foreign_origin", func(r *http.Request) { r.Header.Set("Origin", "https://attacker.example") }, 403},
		{"null_origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403},
		{"duplicate_origin", func(r *http.Request) {
			r.Header.Add("Origin", "https://client.example")
			r.Header.Add("Origin", "https://client.example")
		}, 403},
		{"allowed_origin", func(r *http.Request) { r.Header.Set("Origin", "https://client.example") }, 200},
		{"unsupported_version", func(r *http.Request) { r.Header.Set("MCP-Protocol-Version", "2025-03-26") }, 400},
		{"missing_version", func(r *http.Request) { r.Header.Del("MCP-Protocol-Version") }, 400},
		{"duplicate_version", func(r *http.Request) { r.Header.Add("MCP-Protocol-Version", protocolVersion) }, 400},
		{"method_header_mismatch", func(r *http.Request) { r.Header.Set("Mcp-Method", "tools/call") }, 400},
		{"bad_content_type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415},
		{"bad_accept", func(r *http.Request) { r.Header.Set("Accept", "application/json") }, 400},
		{"query_string", func(r *http.Request) { r.URL.RawQuery = "token=x" }, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, _, body := httpRequest(t, h, http.MethodPost, httpBody("tools/list", nil), tc.edit)
			if status != tc.want {
				t.Fatalf("got %d %s, want %d", status, body, tc.want)
			}
			if strings.Contains(body, "sensitive") {
				t.Fatal("verifier diagnostic leaked")
			}
		})
	}
}

func TestHTTPMetadataValidation(t *testing.T) {
	h := startHTTPTest(t, HTTPOptions{}, nil)
	for _, body := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`,
		strings.Replace(httpBody("tools/list", nil), protocolVersion, "2025-11-25", 1),
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{},"clientInfo":{"name":"test","version":"1"}}}`,
	} {
		status, _, response := httpRequest(t, h, http.MethodPost, body, nil)
		if status < 400 || status > 499 {
			t.Fatalf("invalid/legacy request accepted: %d %s", status, response)
		}
	}
}

func TestHTTPBoundsAndShutdown(t *testing.T) {
	h := startHTTPTest(t, HTTPOptions{MaxRequestBytes: 256}, nil)
	status, _, body := httpRequest(t, h, http.MethodPost, strings.Repeat("x", 257), nil)
	if status != 413 {
		t.Fatalf("byte cap: %d %s", status, body)
	}
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if err := h.Start(t.Context()); err == nil {
		t.Fatal("closed server restarted")
	}
}

func TestHTTPConcurrentLimitAndCancellation(t *testing.T) {
	entered := make(chan struct{}, 1)
	cancelled := make(chan struct{}, 1)
	h := startHTTPTest(t, HTTPOptions{MaxRequests: 1}, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "hold", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			entered <- struct{}{}
			<-ctx.Done()
			cancelled <- struct{}{}
			return nil, ctx.Err()
		})
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	request, err := newHTTPRequest(ctx, h, http.MethodPost, httpBody("tools/call", map[string]any{"name": "hold", "arguments": map[string]any{}}))
	if err != nil {
		t.Fatal(err)
	}
	finished := make(chan error, 1)
	go func() {
		response, err := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
		finished <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	status, _, body := httpRequest(t, h, http.MethodPost, httpBody("tools/list", nil), nil)
	if status != 429 {
		t.Fatalf("capacity: %d %s", status, body)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("HTTP cancellation did not reach handler")
	}
	select {
	case <-finished:
	case <-time.After(2 * time.Second):
		t.Fatal("request did not finish")
	}
	// Close drains the cancelled HTTP handler before returning.
	if err := h.Close(); err != nil {
		t.Fatal(err)
	}
	if len(h.requests) != 0 {
		t.Fatal("request admission slot leaked")
	}
}

func TestHTTPShutdownCancelsHandler(t *testing.T) {
	entered, cancelled := make(chan struct{}), make(chan struct{})
	h := startHTTPTest(t, HTTPOptions{}, func(s *mcp.Server) {
		s.AddTool(&mcp.Tool{Name: "hold", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, _ *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			return nil, ctx.Err()
		})
	})
	ctx, cancel := context.WithTimeout(t.Context(), 4*time.Second)
	defer cancel()
	request, _ := newHTTPRequest(ctx, h, http.MethodPost, httpBody("tools/call", map[string]any{"name": "hold", "arguments": map[string]any{}}))
	go func() {
		response, _ := http.DefaultClient.Do(request)
		if response != nil {
			response.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("tool did not start")
	}
	closed := make(chan struct{})
	go func() { _ = h.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close did not drain handler")
	}
	select {
	case <-cancelled:
	default:
		t.Fatal("handler not cancelled")
	}
}

func TestHTTPUnexpectedListenerFailure(t *testing.T) {
	h := startHTTPTest(t, HTTPOptions{}, nil)
	h.mu.Lock()
	listener := h.listener
	h.mu.Unlock()
	_ = listener.Close()
	select {
	case err := <-h.Errors():
		if err == nil {
			t.Fatal("missing listener error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("listener failure not reported")
	}
	ordinary := startHTTPTest(t, HTTPOptions{}, nil)
	_ = ordinary.Close()
	select {
	case err := <-ordinary.Errors():
		t.Fatalf("ordinary shutdown fault: %v", err)
	default:
	}
}

func TestHTTPConfigurationValidation(t *testing.T) {
	for _, opts := range []HTTPOptions{{Listen: "0.0.0.0:0"}, {Listen: "bad"}, {AllowedHosts: []string{"*.example.com"}}, {AllowedOrigins: []string{"https://example.com/path"}}, {MaxRequests: -1}, {MaxRequestBytes: -1}} {
		if _, err := NewHTTPServer(opts, nil, httpTestVerifier); err == nil {
			t.Fatalf("invalid options accepted: %+v", opts)
		}
	}
	if _, err := NewHTTPServer(HTTPOptions{}, nil, nil); err == nil {
		t.Fatal("nil verifier accepted")
	}
	if _, err := NewHTTPServer(HTTPOptions{Listen: "0.0.0.0:0", AllowRemote: true}, nil, httpTestVerifier); err == nil {
		t.Fatal("remote plaintext listener accepted")
	}
}
