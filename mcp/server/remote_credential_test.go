package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func remoteCredentialStore(t *testing.T) (*service.Service, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secret.json")
	s := service.New(service.Config{Name: "credential-test", SecretFile: path})
	s.HandleSecret("remote.demo.token", ValidateRemoteToken)
	s.HandleSecret("remote.other.token", ValidateRemoteToken)
	if err := s.ApplySetting("remote.demo.token", "fixture-A"); err != nil {
		t.Fatal(err)
	}
	return s, path
}

// 返回旧准备调用是否仍应有效。失败写入和其他键的更新不能误撤销本目标。
func changeRemoteCredential(t *testing.T, s *service.Service, path, change string) bool {
	t.Helper()
	set := func(value string) {
		t.Helper()
		if err := s.ApplySetting("remote.demo.token", value); err != nil {
			t.Fatal(err)
		}
	}
	switch change {
	case "failed_replace", "failed_clear":
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		value := "fixture-B"
		if change == "failed_clear" {
			value = ""
		}
		if err := s.ApplySetting("remote.demo.token", value); err == nil {
			t.Fatal("save unexpectedly succeeded")
		}
		return true
	case "unchanged":
		set("fixture-A")
		return true
	case "other_remote":
		if err := s.ApplySetting("remote.other.token", "fixture-B"); err != nil {
			t.Fatal(err)
		}
		return true
	case "replace":
		set("fixture-B")
	case "round_trip":
		set("fixture-B")
		set("fixture-A")
	case "clear_then_restore":
		set("")
		set("fixture-A")
	default:
		t.Fatal("unknown transition")
	}
	return false
}

func TestRemoteCredentialVersionRejectsStaleCalls(t *testing.T) {
	for _, duringDiscovery := range []bool{false, true} {
		stage := "before_run"
		if duringDiscovery {
			stage = "during_discovery"
		}
		t.Run(stage, func(t *testing.T) {
			for _, change := range []string{"failed_replace", "failed_clear", "unchanged", "other_remote", "replace", "round_trip", "clear_then_restore"} {
				t.Run(change, func(t *testing.T) {
					var calls atomic.Int32
					remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						calls.Add(1)
						return &mcp.CallToolResult{StructuredContent: map[string]any{"value": "ok"}}, nil
					})
					reached, release := make(chan struct{}, 1), make(chan struct{})
					front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if duringDiscovery && r.Header.Get("Mcp-Method") == "tools/list" {
							reached <- struct{}{}
							select {
							case <-release:
							case <-r.Context().Done():
								return
							}
						}
						remote.Config.Handler.ServeHTTP(w, r)
					}))
					defer front.Close()
					ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
					defer cancel()
					s, path := remoteCredentialStore(t)
					i := newTestImports(t, front.URL, 5*time.Second, 16384, s.SettingSnapshot)
					raw, _ := protocol.EncodePayload(map[string]any{"key": "x"})
					p, err := i.Prepare(protocol.JumpPayload{Action: "remote_lookup", Input: raw})
					if err != nil {
						t.Fatal(err)
					}
					defer p.Close()
					done := make(chan error, 1)
					if duringDiscovery {
						go func() { _, err := p.Run(ctx); done <- err }()
						select {
						case <-reached:
						case <-ctx.Done():
							t.Fatal("catalogue barrier not reached")
						}
					}
					allowed := changeRemoteCredential(t, s, path, change)
					if duringDiscovery {
						close(release)
						err = <-done
					} else {
						_, err = p.Run(ctx)
					}
					if allowed {
						if err != nil || calls.Load() != 1 {
							t.Fatalf("valid call rejected: %v, calls=%d", err, calls.Load())
						}
					} else if err == nil || calls.Load() != 0 {
						t.Fatalf("stale call dispatched: %v, calls=%d", err, calls.Load())
					}
				})
			}
		})
	}
}

func TestRemotePreflightRejectsCredentialRoundTrip(t *testing.T) {
	var calls atomic.Int32
	remote := importFixture(t, lookupTool(), func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{}, nil
	})
	s, _ := remoteCredentialStore(t)
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Mcp-Method") == "tools/list" {
			for _, value := range []string{"fixture-B", "fixture-A"} {
				if err := s.ApplySetting("remote.demo.token", value); err != nil {
					t.Error("rotation failed")
					http.Error(w, "rotation failed", 500)
					return
				}
			}
		}
		remote.Config.Handler.ServeHTTP(w, r)
	}))
	defer front.Close()
	i := newTestImports(t, front.URL, 2*time.Second, 16384, s.SettingSnapshot)
	result := i.Check(t.Context())
	if len(result) != 1 || result[0].OK || result[0].Code != "MCP_CHECK_CANCELLED" || calls.Load() != 0 {
		t.Fatalf("stale preflight accepted: %+v, calls=%d", result, calls.Load())
	}
}
