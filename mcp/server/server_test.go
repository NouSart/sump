package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/gorilla/websocket"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const gatewayToken = "gateway-test-secret-01234567890123456789"

// A real WS peer with a deliberately tiny application handler. Production
// center/files/mcp processes are covered separately in tests/mcp_test.go.
func gatewayPeer(t *testing.T, fn func(protocol.Envelope) (any, error)) string {
	t.Helper()
	var mu sync.Mutex
	var conns []*websocket.Conn
	u := websocket.Upgrader{}
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := u.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		mu.Lock()
		conns = append(conns, c)
		mu.Unlock()
		defer c.Close()
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			env, err := protocol.Unmarshal(data)
			if err != nil {
				return
			}
			out, e := fn(env)
			raw, _ := protocol.EncodePayload(out)
			response := protocol.ResponsePayload{OK: e == nil, Data: raw}
			if e != nil {
				response.Error = e.Error()
			}
			res, _ := protocol.NewResponse(env, "fixture", response)
			encoded, _ := protocol.Marshal(res)
			if c.WriteMessage(websocket.BinaryMessage, encoded) != nil {
				return
			}
		}
	}))
	t.Cleanup(func() {
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		h.Close()
	})
	return "ws" + strings.TrimPrefix(h.URL, "http")
}

func startGateway(t *testing.T, cfg Config, cards []protocol.ServiceCard, exports string) *Server {
	t.Helper()
	center := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		if env.Type == protocol.TypeRegister {
			return protocol.RosterPayload{Services: cards, Revision: 1}, nil
		}
		return protocol.SettingsResult{}, nil
	})
	dir := t.TempDir()
	cfg.Listen = "127.0.0.1:0"
	cfg.Center = center
	cfg.HTTP.Listen = "127.0.0.1:0"
	cfg.SecretFile = filepath.Join(dir, "secret.json")
	cfg.ConfigPath = filepath.Join(dir, "mcp.json")
	cfg.Logger = log.New(io.Discard, "", 0)
	if err := os.WriteFile(cfg.ConfigPath, []byte(exports), 0600); err != nil {
		t.Fatal(err)
	}
	secret, _ := json.Marshal(map[string]any{"version": 1, "secrets": map[string]string{"http.token": gatewayToken}})
	if err := os.WriteFile(cfg.SecretFile, secret, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

func gatewayClient(t *testing.T, s *Server, token string) *mcp.ClientSession {
	t.Helper()
	c := mcp.NewClient(&mcp.Implementation{Name: "gateway-test", Version: "1"}, nil)
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	cs, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: s.HTTPURL(), HTTPClient: &http.Client{Transport: httpAuthTransport{token}}, DisableStandaloneSSE: true, MaxRetries: -1}, &mcp.ClientSessionOptions{ProtocolVersion: protocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	if cs.InitializeResult().ProtocolVersion != protocolVersion {
		t.Fatal("unexpected protocol fallback")
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func gatewayCall(t *testing.T, c *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	r, err := c.CallTool(ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestGatewayToolContractAndRevocation(t *testing.T) {
	var calls atomic.Int32
	peer := gatewayPeer(t, func(env protocol.Envelope) (any, error) {
		calls.Add(1)
		if env.Boss != "mcp" || env.From != "mcp" || env.Trace == "" {
			t.Errorf("bad routing: %+v", env)
		}
		return protocol.FileReadResult{ID: "sample", Filename: "sample.txt", Text: strings.Repeat("你好\n<", 10000)}, nil
	})
	s := startGateway(t, Config{}, []protocol.ServiceCard{{Name: "files", Addr: peer, Provides: []protocol.Provide{{Action: "read", Tool: true}}}, {Name: "shell", Addr: peer, Provides: []protocol.Provide{{Action: "run", Async: true, Tool: false}}}}, `{"exports":["files__read"]}`)
	c := gatewayClient(t, s, gatewayToken)
	list, err := c.ListTools(t.Context(), nil)
	if err != nil || len(list.Tools) != 1 || list.Tools[0].Name != "files__read" {
		t.Fatalf("tool:false bypass: %+v %v", list, err)
	}
	if r, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "shell__run", Arguments: map[string]any{"command": "Get-Date"}}); err == nil && !r.IsError {
		t.Fatal("unexported tool callable")
	}
	r := gatewayCall(t, c, "files__read", map[string]any{"id": "sample"})
	if r.IsError {
		t.Fatalf("read failed: %+v", r)
	}
	encoded, _ := json.Marshal(r)
	if len(encoded) > s.cfg.MaxOutputBytes || !strings.Contains(string(encoded), `"truncated":true`) {
		t.Fatal("output not correctly bounded and marked")
	}
	if err := os.WriteFile(s.cfg.ConfigPath, []byte(`{"exports":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.ReloadPolicy(); err != nil {
		t.Fatal(err)
	}
	list, err = c.ListTools(t.Context(), nil)
	if err != nil || len(list.Tools) != 0 {
		t.Fatalf("revocation not listed: %+v %v", list, err)
	}
	if r := gatewayCall(t, c, "files__read", map[string]any{"id": "sample"}); !r.IsError {
		t.Fatal("revoked tool callable")
	}
	if calls.Load() != 1 {
		t.Fatalf("unexpected dispatches: %d", calls.Load())
	}
}

func TestGatewayCallLimitAndRecovery(t *testing.T) {
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	peer := gatewayPeer(t, func(protocol.Envelope) (any, error) {
		entered <- struct{}{}
		<-release
		return protocol.FileReadResult{ID: "x", Text: "ok"}, nil
	})
	s := startGateway(t, Config{MaxCalls: 1, MaxCallsPerPrincipal: 1}, []protocol.ServiceCard{{Name: "files", Addr: peer, Provides: []protocol.Provide{{Action: "read", Tool: true}}}}, `{"exports":["files__read"]}`)
	c := gatewayClient(t, s, gatewayToken)
	var once sync.Once
	defer once.Do(func() { close(release) })
	done := make(chan error, 1)
	go func() {
		_, err := c.CallTool(t.Context(), &mcp.CallToolParams{Name: "files__read", Arguments: map[string]any{"id": "x"}})
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("call not dispatched")
	}
	r := gatewayCall(t, c, "files__read", map[string]any{"id": "x"})
	if !r.IsError {
		t.Fatal("concurrency limit bypassed")
	}
	once.Do(func() { close(release) })
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if r := gatewayCall(t, c, "files__read", map[string]any{"id": "x"}); r.IsError {
		t.Fatal("slot not released")
	}
}

func TestGatewayTimeoutNeverReplays(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	peer := gatewayPeer(t, func(protocol.Envelope) (any, error) {
		calls.Add(1)
		<-release
		return protocol.FileReadResult{ID: "x", Text: "finished after caller timeout"}, nil
	})
	s := startGateway(t, Config{CallTimeout: 50 * time.Millisecond}, []protocol.ServiceCard{{Name: "files", Addr: peer, Provides: []protocol.Provide{{Action: "read", Tool: true}}}}, `{"exports":["files__read"]}`)
	c := gatewayClient(t, s, gatewayToken)
	defer close(release)
	r := gatewayCall(t, c, "files__read", map[string]any{"id": "x"})
	raw, _ := json.Marshal(r.StructuredContent)
	if !r.IsError || !strings.Contains(string(raw), `"phase":"timed_out"`) || !strings.Contains(string(raw), `outcome unknown`) {
		t.Fatalf("timeout claimed outcome: %s", raw)
	}
	if calls.Load() != 1 {
		t.Fatalf("ambiguous execution replayed: %d", calls.Load())
	}
}

func TestGatewaySecretRotationAndErrorRedaction(t *testing.T) {
	peer := gatewayPeer(t, func(protocol.Envelope) (any, error) { return nil, errors.New("backend error " + gatewayToken) })
	s := startGateway(t, Config{}, []protocol.ServiceCard{{Name: "files", Addr: peer, Provides: []protocol.Provide{{Action: "read", Tool: true}}}}, `{"exports":["files__read"]}`)
	c := gatewayClient(t, s, gatewayToken)
	r := gatewayCall(t, c, "files__read", map[string]any{"id": "x"})
	raw, _ := json.Marshal(r)
	if !r.IsError || strings.Contains(string(raw), gatewayToken) || !strings.Contains(string(raw), "backend error") {
		t.Fatalf("wrong error redaction: %s", raw)
	}
	if err := s.svc.ApplySetting("http.token", "replacement-token-01234567890123456789"); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	if _, err := c.ListTools(ctx, nil); err == nil {
		t.Fatal("old token survived rotation")
	}
	if _, err := s.verifyToken(ctx, gatewayToken, nil); err == nil {
		t.Fatal("old token accepted")
	}
}

func TestPolicyAtomicReloadAndDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	s, err := New(Config{ConfigPath: path, SecretFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.policy.Load().allows("shell__run") {
		t.Fatal("dangerous tool enabled by default")
	}
	if err = os.WriteFile(path, []byte(`{"exports":["files__read"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.ReloadPolicy(); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{`{"exports":["unknown"]}`, `{"Exports":[]}`, `{"exports":[],"exports":["shell__run"]}`, `{"exports":null}`} {
		if err = os.WriteFile(path, []byte(invalid), 0600); err != nil {
			t.Fatal(err)
		}
		if s.ReloadPolicy() == nil {
			t.Fatalf("accepted ambiguous config: %s", invalid)
		}
		if !s.policy.Load().allows("files__read") {
			t.Fatal("invalid reload changed current policy")
		}
	}
	if err = os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err = s.ReloadPolicy(); err != nil || s.policy.Load().allows("files__read") {
		t.Fatal("missing config did not revoke exports")
	}
}

func TestFailedSecretPersistenceDoesNotAuthorizeToken(t *testing.T) {
	s := startGateway(t, Config{}, nil, `{"exports":[]}`)
	if err := os.Remove(s.cfg.SecretFile); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(s.cfg.SecretFile, 0700); err != nil {
		t.Fatal(err)
	}
	// The secret destination is deliberately a directory: atomic rename must
	// fail, without depending on OS-specific permission or administrator rules.
	c, err := protocol.Dial(s.WsURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	const replacement = "replacement-secret-01234567890123456789"
	raw, _ := protocol.EncodePayload(protocol.ConfigurePayload{Key: "http.token", Value: replacement})
	env, _ := protocol.NewEnvelope(protocol.TypeJump, "settings-center", "mcp", "", protocol.JumpPayload{Action: "configure", Input: raw})
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	res, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var response protocol.ResponsePayload
	if err = res.DecodePayload(&response); err != nil {
		t.Fatal(err)
	}
	if response.OK {
		t.Fatal("secret persistence unexpectedly succeeded")
	}
	if _, err = s.verifyToken(ctx, replacement, nil); err == nil {
		t.Fatal("rejected credential was authorized")
	}
	// 落盘失败 = 这次轮换没发生：旧凭据必须仍然可用（否则入口会全体 401）。
	if _, err = s.verifyToken(ctx, gatewayToken, nil); err != nil {
		t.Fatalf("failed rotation must leave the previous credential usable: %v", err)
	}
}

func TestGatewayRevocationBeforeDispatch(t *testing.T) {
	var dispatched atomic.Int32
	peer := gatewayPeer(t, func(protocol.Envelope) (any, error) {
		dispatched.Add(1)
		return protocol.FileReadResult{ID: "x", Text: "ok"}, nil
	})
	s := startGateway(t, Config{}, []protocol.ServiceCard{{Name: "files", Addr: peer, Provides: []protocol.Provide{{Action: "read", Tool: true}}}}, `{"exports":["files__read"]}`)
	identity, err := s.verifyToken(t.Context(), gatewayToken, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.svc.ApplySetting("http.token", "replacement-secret-01234567890123456789"); err != nil {
		t.Fatal(err)
	}
	req := &mcp.CallToolRequest{Params: &mcp.CallToolParamsRaw{Name: "files__read", Arguments: json.RawMessage(`{"id":"x"}`)}, Extra: &mcp.RequestExtra{TokenInfo: identity}}
	result := s.callTool(t.Context(), req, s.bindings["files__read"])
	if !result.IsError || dispatched.Load() != 0 {
		t.Fatal("preauthenticated revoked credential dispatched")
	}
}

func TestPolicyImportsRequireRestartWithoutPartialApply(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mcp.json")
	old := exportPolicy{Exports: []string{"files__read"}, Remotes: []RemoteConfig{{ID: "demo", URL: "http://127.0.0.1:1/mcp"}}, Imports: []ImportConfig{lookupImport()}}
	write := func(p exportPolicy) {
		t.Helper()
		raw, _ := json.Marshal(p)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	write(old)
	s, err := New(Config{ConfigPath: path, SecretFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	changed := old
	changed.Exports = []string{}
	changed.Imports = []ImportConfig{lookupImport()}
	changed.Imports[0].Action = "replacement_lookup"
	write(changed)
	if err = s.ReloadPolicy(); err == nil {
		t.Fatal("runtime import replacement accepted")
	}
	if !s.policy.Load().allows("files__read") || s.imports.Provides()[0].Action != "remote_lookup" {
		t.Fatal("mixed config partially applied")
	}
	fresh, err := New(Config{ConfigPath: path, SecretFile: "-"})
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	if fresh.policy.Load().allows("files__read") || fresh.imports.Provides()[0].Action != "replacement_lookup" {
		t.Fatal("restart did not load new declarations")
	}
}
