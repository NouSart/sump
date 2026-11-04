// Package server provides the authenticated, bounded MCP HTTP transport.
package server

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const protocolVersion = "2026-07-28"

type HTTPOptions struct {
	Listen                       string
	TLSCertFile, TLSKeyFile      string
	AllowRemote                  bool
	AllowedHosts, AllowedOrigins []string
	MaxRequests                  int
	MaxRequestBytes              int64
}

// HTTPServer owns a stateless endpoint. Identity comes from authentication on
// every request, never from an SDK session ID.
type HTTPServer struct {
	opts           HTTPOptions
	handler        http.Handler
	requests       chan struct{}
	active         sync.WaitGroup
	mu             sync.Mutex
	listener       net.Listener
	http           *http.Server
	tlsConfig      *tls.Config
	url            string
	closed         bool
	hosts, origins map[string]bool
	done           chan struct{}
	drained        chan struct{}
	errors         chan error
}

// The SDK sets transport cache headers while writing responses. Apply no-store
// when headers are committed so authenticated results cannot become cacheable.
type noStoreResponse struct {
	http.ResponseWriter
	wrote bool
}

func (w *noStoreResponse) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.Header().Set("Cache-Control", "no-store")
	w.ResponseWriter.WriteHeader(status)
}
func (w *noStoreResponse) Write(p []byte) (int, error) {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}
func (w *noStoreResponse) FlushError() error {
	if !w.wrote {
		w.WriteHeader(http.StatusOK)
	}
	return http.NewResponseController(w.ResponseWriter).Flush()
}
func (w *noStoreResponse) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// NewHTTPServer validates static configuration. The service must additionally
// reject remote startup without a provisioned secret; this wrapper cannot infer
// whether the supplied verifier is ready to authenticate callers.
func NewHTTPServer(opts HTTPOptions, configure func(*mcp.Server), verify auth.TokenVerifier) (*HTTPServer, error) {
	if verify == nil {
		return nil, errors.New("MCP token verifier is required")
	}
	if opts.Listen == "" {
		opts.Listen = "127.0.0.1:9903"
	}
	host, _, err := net.SplitHostPort(opts.Listen)
	if err != nil {
		return nil, fmt.Errorf("MCP listen address: %w", err)
	}
	ip := net.ParseIP(host)
	remote := host != "localhost" && (ip == nil || !ip.IsLoopback())
	if remote && !opts.AllowRemote {
		return nil, errors.New("non-loopback MCP listen address requires allow_remote")
	}
	if (opts.TLSCertFile == "") != (opts.TLSKeyFile == "") {
		return nil, errors.New("MCP TLS certificate and key must be configured together")
	}
	if remote && opts.TLSCertFile == "" {
		return nil, errors.New("non-loopback MCP listener requires TLS; reverse proxies must use a loopback backend")
	}
	var tlsConfig *tls.Config
	if opts.TLSCertFile != "" {
		cert, err := tls.LoadX509KeyPair(opts.TLSCertFile, opts.TLSKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load MCP TLS certificate and key: %w", err)
		}
		// Load once at startup: replacing files cannot silently change trust
		// while requests are in flight. Certificate rotation requires restart.
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{cert}}
	}
	if opts.MaxRequests == 0 {
		opts.MaxRequests = 32
	}
	if opts.MaxRequestBytes == 0 {
		opts.MaxRequestBytes = 256 << 10
	}
	if opts.MaxRequests < 1 || opts.MaxRequestBytes < 1 {
		return nil, errors.New("MCP resource limits must be positive")
	}
	h := &HTTPServer{opts: opts, tlsConfig: tlsConfig, requests: make(chan struct{}, opts.MaxRequests), hosts: make(map[string]bool), origins: make(map[string]bool), done: make(chan struct{}), drained: make(chan struct{}), errors: make(chan error, 1)}
	for _, host := range opts.AllowedHosts {
		if host == "" || strings.ContainsAny(host, "/\\@?# \t\r\n") || strings.Contains(host, "*") {
			return nil, errors.New("allowed_hosts must contain exact HTTP host authorities")
		}
		h.hosts[strings.ToLower(host)] = true
	}
	for _, origin := range opts.AllowedOrigins {
		u, e := url.Parse(origin)
		if e != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
			return nil, errors.New("allowed_origins must contain exact http(s) origins")
		}
		h.origins[origin] = true
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "sump-mcp", Version: "0.2.0"}, &mcp.ServerOptions{
		SupportedProtocolVersions: []string{protocolVersion},
		SetCacheable: func(_ context.Context, _ mcp.Request, c *mcp.Cacheable) {
			c.CacheScope, c.TTLMs = "private", 0
		},
	})
	if configure != nil {
		configure(server)
	}
	sdk := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Stateless: true, JSONResponse: true, PropagateRequestCancellation: true,
		MaxRequestBodyBytes: opts.MaxRequestBytes, DisableLocalhostProtection: true,
	})
	// Never expose verifier errors, which may contain credential material.
	checkedVerifier := func(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
		info, err := verify(ctx, token, req)
		if err != nil || info == nil || info.UserID == "" {
			return nil, auth.ErrInvalidToken
		}
		return info, nil
	}
	protected := auth.RequireBearerToken(checkedVerifier, &auth.RequireBearerTokenOptions{AllowMissingExpiration: true})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.serveAuthenticated(w, r, sdk)
	}))
	h.handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w = &noStoreResponse{ResponseWriter: w}
		w.Header().Set("Cache-Control", "no-store")
		// Admission is locked against Close's drain. Capacity includes slow
		// bodies, without a wait queue that could grow without bound.
		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			http.Error(w, "MCP server closed", http.StatusServiceUnavailable)
			return
		}
		select {
		case h.requests <- struct{}{}:
			h.active.Add(1)
		default:
			h.mu.Unlock()
			http.Error(w, "MCP request capacity reached", http.StatusTooManyRequests)
			return
		}
		h.mu.Unlock()
		defer func() { <-h.requests; h.active.Done() }()
		if len(r.Header.Values("Authorization")) != 1 {
			http.Error(w, "bearer token required", http.StatusUnauthorized)
			return
		}
		protected.ServeHTTP(w, r)
	})
	return h, nil
}

// Start binds synchronously. Cancelling ctx closes HTTP and cancels in-flight
// handlers; this is not a guarantee of cancellation of remote side effects.
func (h *HTTPServer) Start(ctx context.Context) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return errors.New("MCP HTTP server is closed")
	}
	if h.listener != nil {
		return errors.New("MCP HTTP server already started")
	}
	listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", h.opts.Listen)
	if err != nil {
		return err
	}
	h.listener = listener
	host, port, _ := net.SplitHostPort(listener.Addr().String())
	for _, allowed := range []string{"localhost", "127.0.0.1", "::1"} {
		h.hosts[strings.ToLower(net.JoinHostPort(allowed, port))] = true
	}
	if ip := net.ParseIP(host); ip != nil && !ip.IsUnspecified() {
		h.hosts[strings.ToLower(net.JoinHostPort(host, port))] = true
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		host = "127.0.0.1"
	}
	scheme := "http"
	if h.tlsConfig != nil {
		scheme = "https"
	}
	h.url = scheme + "://" + net.JoinHostPort(host, port) + "/mcp"
	h.http = &http.Server{Handler: h.handler, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	if h.tlsConfig != nil {
		h.http.TLSConfig = h.tlsConfig.Clone()
	}
	go func() {
		var err error
		if h.tlsConfig != nil {
			// ServeTLS uses the already loaded Certificates, not the file paths.
			err = h.http.ServeTLS(listener, "", "")
		} else {
			err = h.http.Serve(listener)
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.errors <- errors.New("MCP HTTP listener stopped unexpectedly; restart required")
		}
		_ = h.Close()
	}()
	go func() {
		select {
		case <-ctx.Done():
			_ = h.Close()
		case <-h.done:
		}
	}()
	return nil
}

func (h *HTTPServer) URL() string { h.mu.Lock(); defer h.mu.Unlock(); return h.url }

// Errors reports unexpected listener failure once; ordinary shutdown is silent.
func (h *HTTPServer) Errors() <-chan error { return h.errors }

func (h *HTTPServer) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		<-h.drained
		return nil
	}
	h.closed = true
	close(h.done)
	srv := h.http
	h.mu.Unlock()
	var err error
	if srv != nil {
		err = srv.Close()
	}
	h.active.Wait()
	close(h.drained)
	return err
}

func (h *HTTPServer) serveAuthenticated(w http.ResponseWriter, r *http.Request, sdk http.Handler) {
	h.mu.Lock()
	hostOK := h.hosts[strings.ToLower(r.Host)]
	h.mu.Unlock()
	if !hostOK {
		http.Error(w, "Host not allowed", http.StatusForbidden)
		return
	}
	origins := r.Header.Values("Origin")
	if len(origins) > 1 || (len(origins) == 1 && !h.origins[origins[0]]) {
		http.Error(w, "Origin not allowed", http.StatusForbidden)
		return
	}
	if r.URL.Path != "/mcp" || r.URL.RawQuery != "" {
		http.NotFound(w, r)
		return
	}
	// Requiring a version header prevents entering the SDK's legacy initialize
	// path when a caller omits new-protocol metadata.
	if r.Method == http.MethodPost && (len(r.Header.Values("MCP-Protocol-Version")) != 1 || r.Header.Get("MCP-Protocol-Version") == "") {
		http.Error(w, "one MCP-Protocol-Version header is required", http.StatusBadRequest)
		return
	}
	// SDK validates JSON-RPC, per-request metadata, method/parameter headers,
	// content negotiation, and request size. It never emits session IDs.
	sdk.ServeHTTP(w, r)
}
