package server

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCheckRemoteConfigUsesConfigurationWithoutStartingSUMP(t *testing.T) {
	var calls atomic.Int32
	endpoint := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "unexpected business execution"}}, nil
	})
	dir := t.TempDir()
	policy, _ := json.Marshal(exportPolicy{Exports: []string{}, Remotes: []RemoteConfig{{ID: "demo", URL: endpoint.URL, TokenRequired: true}}, Imports: []ImportConfig{lookupImport()}})
	configPath, secretPath := filepath.Join(dir, "mcp.json"), filepath.Join(dir, "secret.json")
	if err := os.WriteFile(configPath, policy, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(secretPath, []byte(`{"version":1,"secrets":{"remote.demo.token":"private-value-never-in-report"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	// Neither this invalid listen address nor an unavailable center may matter
	// to a read-only outgoing check. No native service is started.
	report, err := CheckRemoteConfig(t.Context(), Config{Listen: "invalid-listener", Center: "ws://127.0.0.1:1/ws", ConfigPath: configPath, SecretFile: secretPath, CallTimeout: time.Second})
	if err != nil || !report.OK || len(report.Checks) != 1 || !report.Checks[0].OK {
		t.Fatalf("check failed: %+v %v", report, err)
	}
	raw, _ := json.Marshal(report)
	if calls.Load() != 0 || strings.Contains(string(raw), "private-value") {
		t.Fatal("check invoked business or leaked credential")
	}
	if err := os.Remove(secretPath); err != nil {
		t.Fatal(err)
	}
	report, err = CheckRemoteConfig(t.Context(), Config{ConfigPath: configPath, SecretFile: secretPath})
	if err != nil || report.OK || report.Checks[0].Code != "MCP_AUTH_REQUIRED" || calls.Load() != 0 {
		t.Fatalf("missing required credential not rejected: %+v %v", report, err)
	}
}

func TestCheckSecretsRejectMalformedWithoutEchoingContent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secret.json")
	for _, body := range []string{
		`{"version":2,"secrets":{"x":"never-echo-this"}}`,
		`{"version":1,"secrets":{"x":"never-echo-this","x":"duplicate"}}`,
		`{"version":1,"secrets":{"x":"never-echo-this"},"surprise":true}`,
		strings.Repeat("never-echo-this", 10000),
	} {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		_, err := readCheckSecrets(path)
		if err == nil || strings.Contains(err.Error(), "never-echo-this") {
			t.Fatalf("invalid or leaky diagnostic: %v", err)
		}
	}
	if _, err := CheckRemoteConfig(t.Context(), Config{ConfigPath: filepath.Join(t.TempDir(), "absent.json"), SecretFile: "-"}); err == nil {
		t.Fatal("empty configuration claimed success")
	}
}
