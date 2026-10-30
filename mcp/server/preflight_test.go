package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestImportCheckAuthenticatesWithoutCallingTools(t *testing.T) {
	var calls, authorized atomic.Int32
	fixture := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "checked"}}, nil
	})
	const token = "preflight-private-credential"
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			http.Error(w, "credential rejected: "+token, http.StatusUnauthorized)
			return
		}
		authorized.Add(1)
		fixture.Config.Handler.ServeHTTP(w, r)
	}))
	defer endpoint.Close()
	a, b := lookupImport(), lookupImport()
	b.Action = "aaa_lookup"
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: endpoint.URL, TokenRequired: true}}, []ImportConfig{a, b}, time.Second, 16384, func(string) (string, uint64) { return token, 0 })
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	got := i.Check(t.Context())
	if len(got) != 2 || got[0].Action != "aaa_lookup" || got[1].Action != "remote_lookup" || !got[0].OK || !got[1].OK || authorized.Load() < 4 || calls.Load() != 0 {
		t.Fatalf("preflight: %+v authorized=%d calls=%d", got, authorized.Load(), calls.Load())
	}
	encoded, _ := json.Marshal(got)
	if strings.Contains(string(encoded), token) {
		t.Fatal("check leaked credential")
	}
	if _, err := callImport(i, t.Context(), "x"); err != nil || calls.Load() != 1 {
		t.Fatalf("actual invocation count: %v / %d", err, calls.Load())
	}
	for _, secret := range []string{"", "wrong"} {
		other, err := NewImports([]RemoteConfig{{ID: "demo", URL: endpoint.URL, TokenRequired: true}}, []ImportConfig{a}, time.Second, 16384, func(string) (string, uint64) { return secret, 0 })
		if err != nil {
			t.Fatal(err)
		}
		result := other.Check(t.Context())
		other.Close()
		encoded, _ = json.Marshal(result)
		if result[0].OK || strings.Contains(string(encoded), token) || calls.Load() != 1 {
			t.Fatalf("invalid credential check: %+v", result)
		}
	}
}

func TestImportCheckDistinguishesUnavailableAndChangedContract(t *testing.T) {
	broken := httptest.NewServer(http.NotFoundHandler())
	defer broken.Close()
	var calls atomic.Int32
	remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	a, b := lookupImport(), lookupImport()
	a.Action, a.Remote = "broken_lookup", "broken"
	b.OutputSchema = nil
	i, err := NewImports([]RemoteConfig{{ID: "broken", URL: broken.URL}, {ID: "demo", URL: remote.URL}}, []ImportConfig{a, b}, time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	got := i.Check(t.Context())
	if got[0].Code != "MCP_REMOTE_UNAVAILABLE" || got[1].Code != "MCP_CONTRACT_MISMATCH" || calls.Load() != 0 {
		t.Fatalf("fault categories: %+v", got)
	}
}

func TestImportCheckRejectsNegotiatedOldProtocol(t *testing.T) {
	var calls atomic.Int32
	server := mcp.NewServer(&mcp.Implementation{Name: "old-server", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
	server.AddTool(lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true}))
	defer remote.Close()
	i := newTestImports(t, remote.URL, time.Second, 16384, nil)
	if got := i.Check(t.Context()); got[0].OK || got[0].Code != "MCP_PROTOCOL_MISMATCH" || calls.Load() != 0 {
		t.Fatalf("old protocol authorized business call: %+v", got)
	}
}

func TestImportCheckSharesRuntimePagination(t *testing.T) {
	for _, cycle := range []bool{false, true} {
		t.Run(map[bool]string{false: "next_page", true: "cursor_cycle"}[cycle], func(t *testing.T) {
			var lists, calls atomic.Int32
			remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var req struct {
					ID     json.RawMessage `json:"id"`
					Method string          `json:"method"`
					Params struct {
						Cursor string `json:"cursor"`
					} `json:"params"`
				}
				if json.NewDecoder(r.Body).Decode(&req) != nil {
					http.Error(w, "bad request", 400)
					return
				}
				var result any
				switch req.Method {
				case "server/discover":
					result = map[string]any{"supportedVersions": []string{importProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "_meta": map[string]any{"io.modelcontextprotocol/serverInfo": map[string]any{"name": "raw-pages", "version": "1"}}}
				case "initialize":
					result = map[string]any{"protocolVersion": importProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "raw-pages", "version": "1"}}
				case "tools/list":
					lists.Add(1)
					if req.Params.Cursor == "" || cycle {
						result = map[string]any{"tools": []any{}, "nextCursor": "next"}
					} else {
						result = map[string]any{"tools": []any{lookupTool()}}
					}
				case "tools/call":
					calls.Add(1)
					result = map[string]any{"content": []any{}, "structuredContent": map[string]any{"value": "actual"}}
				default:
					w.WriteHeader(202)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer remote.Close()
			i := newTestImports(t, remote.URL, time.Second, 16384, nil)
			got := i.Check(t.Context())
			if got[0].OK == cycle || lists.Load() != 2 || calls.Load() != 0 {
				t.Fatalf("pagination preflight: %+v lists=%d calls=%d", got, lists.Load(), calls.Load())
			}
			if !cycle {
				if _, err := callImport(i, t.Context(), "x"); err != nil || calls.Load() != 1 || lists.Load() != 4 {
					t.Fatalf("runtime disagrees with preflight: %v", err)
				}
			}
		})
	}
}

func TestImportCheckBoundsLifetimeAndSharesRemoteCapacity(t *testing.T) {
	var requests atomic.Int32
	release := make(chan struct{})
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer remote.Close()
	defer close(release)
	i := newTestImports(t, remote.URL, 50*time.Millisecond, 16384, nil)
	raw, _ := protocol.EncodePayload(map[string]any{"key": "held"})
	var held []*preparedImport
	for range 4 {
		p, err := i.Prepare(protocol.JumpPayload{Action: "remote_lookup", Input: raw})
		if err != nil {
			t.Fatal(err)
		}
		held = append(held, p)
		defer p.Close()
	}
	if got := i.Check(t.Context()); got[0].Code != "MCP_BUSY" || requests.Load() != 0 {
		t.Fatalf("check ignored normal slots: %+v", got)
	}
	for _, p := range held {
		p.Close()
	}
	start := time.Now()
	if got := i.Check(t.Context()); got[0].OK {
		t.Fatal("blocked endpoint checked successfully")
	}
	if time.Since(start) > time.Second {
		t.Fatal("check ignored total timeout")
	}
	i.Close()
	if got := i.Check(t.Context()); got[0].OK {
		t.Fatal("closed imports checked successfully")
	}
}
