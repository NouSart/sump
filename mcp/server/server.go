// Package server translates explicit capabilities between MCP and SUMP.
// The settings center discovers one service; external servers stay behind it.
package server

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type credential struct {
	value string
	hash  [32]byte
	owner string
}

type Server struct {
	cfg             Config
	svc             *service.Service
	http            *HTTPServer
	imports         *Imports
	bindings        map[string]Binding
	policy          atomic.Pointer[exportPolicy]
	startup         *exportPolicy
	token           atomic.Pointer[credential]
	mu              sync.Mutex
	started, closed bool
	cancel          context.CancelFunc
	calls           int
	principalCalls  map[string]int
	lifetime        context.Context
	asyncImports    *asyncImports
	jobs            *Jobs
	controls        int
}

func New(cfg Config) (*Server, error) {
	if err := cfg.defaults(); err != nil {
		return nil, err
	}
	s := &Server{cfg: cfg, bindings: BuiltinBindings(), principalCalls: make(map[string]int), asyncImports: newAsyncImports()}
	s.jobs = NewJobs(JobsConfig{MaxJobs: cfg.MaxJobs, MaxBytes: cfg.MaxOutputBytes / 4, MaxEarlyReports: 8, TTL: cfg.JobTTL, Timeout: cfg.AsyncTimeout})
	p, err := readPolicy(cfg.ConfigPath, s.bindings)
	if err != nil {
		return nil, err
	}
	s.startup = p
	s.policy.Store(p)
	s.imports, err = NewImports(p.Remotes, p.Imports, cfg.CallTimeout, cfg.MaxOutputBytes, func(key string) (string, uint64) { return s.svc.SettingSnapshot(key) })
	if err != nil {
		return nil, err
	}
	s.svc = service.New(service.Config{Name: "mcp", Title: "MCP 协议翻译", Listen: cfg.Listen, Center: cfg.Center, Logger: cfg.Logger, SecretFile: cfg.SecretFile, MaxMessageBytes: cfg.MaxMessageBytes,
		ActionTimeout: cfg.CallTimeout,
		Description:   "Translate explicitly enabled capabilities between SUMP and MCP.", Provides: s.imports.Provides(),
	})
	s.svc.HandleSecretWithCommit("http.token", validateToken, s.commitToken)
	for _, r := range p.Remotes {
		s.svc.HandleSecret(r.SecretKey(), ValidateRemoteToken)
	}
	for _, item := range [][2]string{{"name", "mcp"}, {"addr", cfg.Listen}, {"center", cfg.Center}, {"http.addr", cfg.HTTP.Listen}, {"config.path", cfg.ConfigPath}} {
		s.svc.HandleReadOnly(item[0], item[1])
	}
	for _, provide := range s.imports.Provides() {
		s.svc.Handle(provide.Action, s.handleImport)
	}
	s.svc.Handle("report_result", s.handleReport)
	s.svc.Handle("cancel_import", s.handleCancelImport)
	s.http, err = NewHTTPServer(cfg.HTTP, s.configureMCP, s.verifyToken)
	if err != nil {
		s.imports.Close()
		return nil, err
	}
	return s, nil
}

func (s *Server) Start(ctx context.Context) error {
	s.mu.Lock()
	if s.started || s.closed {
		s.mu.Unlock()
		return errors.New("MCP service already started or closed")
	}
	s.started = true
	ctx, s.cancel = context.WithCancel(ctx)
	s.lifetime = ctx
	s.mu.Unlock()
	if err := s.svc.Start(ctx); err != nil {
		s.Close()
		return err
	}
	host, _, _ := net.SplitHostPort(s.cfg.HTTP.Listen)
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) && s.token.Load() == nil {
		s.Close()
		return errors.New("remote MCP listener requires a provisioned http.token secret")
	}
	if err := s.http.Start(ctx); err != nil {
		s.svc.Report("internal", "MCP HTTP listener failed to start")
		s.Close()
		return err
	}
	for _, remote := range s.startup.Remotes {
		if strings.TrimRight(remote.URL, "/") == strings.TrimRight(s.HTTPURL(), "/") {
			s.Close()
			return errors.New("MCP cannot import its own endpoint")
		}
	}
	go s.maintain(ctx)
	return nil
}

func (s *Server) Close() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	cancel := s.cancel
	s.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	s.jobs.Close()
	s.imports.Close()
	_ = s.http.Close()
	drainCtx, drainCancel := context.WithTimeout(context.Background(), s.cfg.DeliveryTimeout+time.Second)
	if err := s.asyncImports.Close(drainCtx); err != nil {
		s.cfg.Logger.Printf("MCP async shutdown: %v", err)
	}
	drainCancel()
	s.svc.Shutdown()
}

func (s *Server) WsURL() string   { return s.svc.WsURL() }
func (s *Server) HTTPURL() string { return s.http.URL() }

// ReloadPolicy atomically replaces export permissions. Imports and destinations
// are startup declarations; changing them requires restart, never partial apply.
// Removing the file revokes exports but does not unregister running imports.
func (s *Server) ReloadPolicy() error {
	p, err := readPolicy(s.cfg.ConfigPath, s.bindings)
	if err != nil {
		return err
	}
	if _, err := os.Stat(s.cfg.ConfigPath); errors.Is(err, os.ErrNotExist) {
		p.Remotes, p.Imports = s.startup.Remotes, s.startup.Imports
	}
	if !reflect.DeepEqual(p.Remotes, s.startup.Remotes) || !reflect.DeepEqual(p.Imports, s.startup.Imports) {
		return errors.New("MCP imports or remotes changed; restart required")
	}
	s.mu.Lock()
	s.policy.Store(p)
	s.mu.Unlock()
	return nil
}

func (s *Server) maintain(ctx context.Context) {
	t := time.NewTicker(s.cfg.ReloadInterval)
	defer t.Stop()
	lastError := ""
	for {
		select {
		case <-ctx.Done():
			s.Close()
			return
		case err := <-s.http.Errors():
			s.svc.Report("internal", err.Error())
		case <-t.C:
			s.jobs.Sweep()
			message := ""
			if err := s.ReloadPolicy(); err != nil {
				message = err.Error()
			}
			if message != "" && message != lastError {
				s.svc.Report("internal", message)
			}
			lastError = message
		}
	}
}

func validateToken(value string) error {
	if value != "" && (len(value) < 32 || len(value) > 4096 || strings.ContainsAny(value, " \t\r\n")) {
		return errors.New("http.token must be 32–4096 bytes without whitespace")
	}
	return nil
}

// 秘密保存成功后才切换身份；失败轮换不会丢失既有任务的归属。
// 成功改回曾经用过的值仍生成新身份，不能恢复旧任务的访问权。
func (s *Server) commitToken(value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.token.Load()
	if old != nil && old.value == value {
		return
	}
	if value == "" {
		s.token.Store(nil)
	} else {
		s.token.Store(&credential{value: value, hash: sha256.Sum256([]byte(value)), owner: "http:" + protocol.NewID()})
	}
}

func (s *Server) verifyToken(_ context.Context, value string, _ *http.Request) (*auth.TokenInfo, error) {
	c := s.token.Load()
	hash := sha256.Sum256([]byte(value))
	// 发布期间仍核对骨架的已提交值，防止旧身份通过后续派发检查。
	if c == nil || subtle.ConstantTimeCompare(hash[:], c.hash[:]) != 1 || s.svc.SettingValue("http.token") != c.value {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{UserID: c.owner}, nil
}

func (s *Server) available(b Binding) bool {
	if !s.policy.Load().allows(b.Name) {
		return false
	}
	for _, card := range s.svc.Roster() {
		if card.Name != b.Service {
			continue
		}
		for _, p := range card.Provides {
			if p.Action == b.Action {
				return p.Tool && p.Async == b.Async
			}
		}
	}
	return false
}

func (s *Server) configureMCP(ms *mcp.Server) {
	s.configureJobResources(ms)
	for _, b := range s.bindings {
		ms.AddTool(b.Tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return s.callTool(ctx, req, b), nil
		})
	}
	ms.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/list" {
				out := &mcp.ListToolsResult{Cacheable: mcp.Cacheable{CacheScope: "private", TTLMs: 0}, Tools: []*mcp.Tool{}}
				for _, b := range s.bindings {
					if s.available(b) {
						out.Tools = append(out.Tools, b.Tool)
					}
				}
				sort.Slice(out.Tools, func(i, j int) bool { return out.Tools[i].Name < out.Tools[j].Name })
				return out, nil
			}
			return next(ctx, method, req)
		}
	})
}

// Admission is the local commitment to dispatch. Revocation before admission
// wins; admitted work can still reach the backend after a subsequent revocation.
func (s *Server) acquire(principal string, b *Binding) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("MCP service closed")
	}
	if b != nil {
		c := s.token.Load()
		if c == nil || c.owner != principal || s.svc.SettingValue("http.token") != c.value {
			return errors.New("credential revoked")
		}
		if !s.available(*b) {
			return errors.New("tool is not enabled or its SUMP contract is unavailable")
		}
	}
	if s.calls >= s.cfg.MaxCalls || s.principalCalls[principal] >= s.cfg.MaxCallsPerPrincipal {
		return errors.New("MCP call concurrency limit reached")
	}
	s.calls++
	s.principalCalls[principal]++
	return nil
}
func (s *Server) release(principal string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls--
	s.principalCalls[principal]--
	if s.principalCalls[principal] == 0 {
		delete(s.principalCalls, principal)
	}
}

func (s *Server) handleImport(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	if s.imports.IsAsync(in.Action) {
		return s.handleAsyncImport(ctx, env, in)
	}
	// SUMP From is routing identity within the trusted internal network, not a
	// credential. External HTTP callers cannot choose it or invoke imports here.
	principal := "sump:" + env.From
	if err := s.acquire(principal, nil); err != nil {
		return nil, err
	}
	defer s.release(principal)
	return s.imports.Handle(ctx, env, in)
}

func (s *Server) callTool(ctx context.Context, req *mcp.CallToolRequest, b Binding) *mcp.CallToolResult {
	if req.Extra == nil || req.Extra.TokenInfo == nil {
		return s.failure("rejected", "authenticated principal required")
	}
	input, err := b.DecodeInput(req.Params.Arguments)
	if err != nil {
		return s.failure("rejected", err.Error())
	}
	principal := req.Extra.TokenInfo.UserID
	if b.Name == "timer__cancel" {
		return s.cancelTimer(ctx, principal, b, input.(string))
	}
	if err = s.acquire(principal, &b); err != nil {
		return s.failure("rejected", err.Error())
	}
	if b.Async {
		return s.callAsyncTool(ctx, principal, b, input)
	}
	defer s.release(principal)
	callCtx, cancel := context.WithTimeout(ctx, s.cfg.CallTimeout)
	defer cancel()
	raw, err := s.svc.Call(callCtx, b.Service, b.Action, input, protocol.NewID(), "mcp")
	if err != nil {
		phase := "failed"
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			phase = "timed_out"
		}
		return s.failure(phase, "backend outcome unknown; no automatic retry: "+err.Error())
	}
	result, err := b.DecodeOutput(raw, s.cfg.MaxOutputBytes/8)
	if err != nil {
		s.svc.Report("internal", "MCP adapter received an incompatible SUMP result")
		return s.failure("failed", err.Error())
	}
	return s.result(result, false)
}

func (s *Server) result(value map[string]any, failed bool) *mcp.CallToolResult {
	raw, err := json.Marshal(value)
	if err != nil {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "result encoding failed"}}}
	}
	res := &mcp.CallToolResult{IsError: failed, StructuredContent: value, Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}}}
	wire, err := json.Marshal(res)
	if err != nil || len(wire) > s.cfg.MaxOutputBytes {
		return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "result exceeds MCP output limit"}}}
	}
	return res
}
func (s *Server) failure(phase, reason string) *mcp.CallToolResult {
	if c := s.token.Load(); c != nil {
		reason = strings.ReplaceAll(reason, c.value, "[redacted]")
	}
	for _, r := range s.startup.Remotes {
		if token := s.svc.SettingValue(r.SecretKey()); token != "" {
			reason = strings.ReplaceAll(reason, token, "[redacted]")
		}
	}
	return s.result(map[string]any{"phase": phase, "reason": utf8Prefix(reason, 2048)}, true)
}
