package server

import (
	"context"
	"encoding/pem"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func importTLSFixture(t *testing.T, calls *atomic.Int32) (*httptest.Server, string) {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "tls-remote", Version: "1"}, &mcp.ServerOptions{SupportedProtocolVersions: []string{importProtocolVersion}})
	server.AddTool(lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "tls"}}, nil
	})
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true})
	remote := httptest.NewUnstartedServer(h)
	remote.Config.ErrorLog = log.New(io.Discard, "", 0)
	remote.StartTLS()
	t.Cleanup(remote.Close)
	ca := filepath.Join(t.TempDir(), "remote-ca.pem")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: remote.Certificate().Raw}), 0600); err != nil {
		t.Fatal(err)
	}
	return remote, ca
}

func TestImportsTLSExplicitRootsAreDestinationScoped(t *testing.T) {
	var calls atomic.Int32
	remote, ca := importTLSFixture(t, &calls)
	trusted, system := lookupImport(), lookupImport()
	system.Action, system.Remote = "system_lookup", "system"
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: remote.URL, CAFile: ca}, {ID: "system", URL: remote.URL}}, []ImportConfig{trusted, system}, 2*time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	checks := i.Check(t.Context())
	if !checks[0].OK || checks[1].OK || checks[1].Code != "MCP_REMOTE_UNAVAILABLE" || calls.Load() != 0 {
		t.Fatalf("CA trust leaked or preflight executed: %+v, %d", checks, calls.Load())
	}
	if _, err = callImport(i, t.Context(), "x"); err != nil || calls.Load() != 1 {
		t.Fatalf("trusted TLS call: %v (%d)", err, calls.Load())
	}
	raw, _ := protocol.EncodePayload(map[string]any{"key": "x"})
	if _, err = i.Handle(t.Context(), protocol.Envelope{}, protocol.JumpPayload{Action: "system_lookup", Input: raw}); err == nil || calls.Load() != 1 {
		t.Fatal("unknown authority reached tool")
	}
	if i.remotes["demo"].transport == i.remotes["system"].transport || i.remotes["system"].transport.TLSClientConfig.RootCAs != nil || i.remotes["demo"].transport.TLSClientConfig.InsecureSkipVerify {
		t.Fatal("transport trust configuration leaked")
	}
}

func TestImportsTLSStillVerifiesHostname(t *testing.T) {
	var calls atomic.Int32
	remote, ca := importTLSFixture(t, &calls)
	if remote.Certificate().VerifyHostname("localhost") == nil {
		t.Fatal("fixture unexpectedly allows localhost")
	}
	i, err := NewImports([]RemoteConfig{{ID: "demo", URL: strings.Replace(remote.URL, "127.0.0.1", "localhost", 1), CAFile: ca}}, []ImportConfig{lookupImport()}, 2*time.Second, 16384, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer i.Close()
	if got := i.Check(t.Context()); got[0].OK || got[0].Code != "MCP_REMOTE_UNAVAILABLE" {
		t.Fatalf("hostname mismatch accepted: %+v", got)
	}
	if _, err = callImport(i, t.Context(), "x"); err == nil || calls.Load() != 0 {
		t.Fatal("hostname mismatch reached tool")
	}
}

func TestImportsRejectInvalidCABundles(t *testing.T) {
	remote := RemoteConfig{ID: "demo", URL: "https://127.0.0.1:1", CAFile: filepath.Join(t.TempDir(), "ca.pem")}
	if _, err := NewImports([]RemoteConfig{remote}, nil, time.Second, 16384, nil); err == nil {
		t.Fatal("missing CA accepted")
	}
	for _, contents := range []string{"not a certificate", strings.Repeat("x", (1<<20)+1)} {
		if err := os.WriteFile(remote.CAFile, []byte(contents), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewImports([]RemoteConfig{remote}, nil, time.Second, 16384, nil); err == nil {
			t.Fatal("invalid CA accepted")
		}
	}
	remote.URL = "http://127.0.0.1:1"
	if ValidateImportConfigs([]RemoteConfig{remote}, nil) == nil {
		t.Fatal("ca_file accepted for plaintext HTTP")
	}
}
