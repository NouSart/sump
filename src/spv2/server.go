package spv2

import (
	"context"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Handler 处理一次 call/event。call 必须通过 Call.Reply / ReplyError / SendStream 回应。
type Handler func(ctx context.Context, call *Call) error

// ServerOptions 是服务端选项。
type ServerOptions struct {
	MaxMessageBytes int64
	ActionTimeout   time.Duration
	MaxInflight     int
	Metrics         *Registry
	Idempotency     *IdempotencyCache
}

// ServerOption 修改 ServerOptions。
type ServerOption func(*ServerOptions)

func WithMaxMessageBytes(n int64) ServerOption {
	return func(o *ServerOptions) { o.MaxMessageBytes = n }
}
func WithActionTimeout(d time.Duration) ServerOption {
	return func(o *ServerOptions) { o.ActionTimeout = d }
}
func WithMaxInflight(n int) ServerOption {
	return func(o *ServerOptions) { o.MaxInflight = n }
}
func WithMetrics(r *Registry) ServerOption {
	return func(o *ServerOptions) { o.Metrics = r }
}

// WithIdempotency 启用幂等缓存。
func WithIdempotency(c *IdempotencyCache) ServerOption {
	return func(o *ServerOptions) { o.Idempotency = c }
}

// Server 是一个 SPv2.1 服务端。
type Server struct {
	card     Card
	handlers map[string]Handler
	opts     ServerOptions
	metrics  *Registry
	idem     *IdempotencyCache
	sem      chan struct{}
	upgrader websocket.Upgrader
	started  time.Time
}

// NewServer 创建服务端。
func NewServer(card Card, opts ...ServerOption) (*Server, error) {
	if err := card.Validate(); err != nil {
		return nil, err
	}
	o := ServerOptions{MaxMessageBytes: 4 << 20, ActionTimeout: 30 * time.Second, MaxInflight: 64}
	for _, opt := range opts {
		opt(&o)
	}
	if o.Metrics == nil {
		o.Metrics = NewRegistry()
	}
	s := &Server{
		card: card, handlers: map[string]Handler{}, opts: o, metrics: o.Metrics, idem: o.Idempotency,
		sem: make(chan struct{}, o.MaxInflight), started: time.Now(),
		upgrader: websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }},
	}
	s.Handle("hello", s.handleHello)
	s.Handle("health", s.handleHealth)
	s.Handle("metrics", s.handleMetrics)
	return s, nil
}

// Card 返回服务卡。
func (s *Server) Card() Card { return s.card }

// Metrics 返回指标注册表。
func (s *Server) Metrics() *Registry { return s.metrics }

// Handle 注册 method。
func (s *Server) Handle(name string, h Handler) { s.handlers[name] = h }

// Handler 返回 HTTP handler（挂到 /ws）。
func (s *Server) Handler() http.Handler { return http.HandlerFunc(s.serveWS) }

func (s *Server) serveWS(w http.ResponseWriter, r *http.Request) {
	ws, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &conn{ws: ws}
	defer c.Close()
	if s.opts.MaxMessageBytes > 0 {
		ws.SetReadLimit(s.opts.MaxMessageBytes)
	}
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		f, err := Unmarshal(data)
		if err != nil {
			return
		}
		s.dispatch(c, f)
	}
}

func (s *Server) dispatch(c *conn, f Frame) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		if f.Type == TypeCall {
			_ = c.send(NewErrorReply(f, s.card.Actor, codeOf(err), err.Error(), retryableOf(err), detailsOf(err)))
		}
		return
	}
	if err := ValidateTo(f, s.card.Actor); err != nil {
		if f.Type == TypeCall {
			_ = c.send(NewErrorReply(f, s.card.Actor, codeOf(err), err.Error(), retryableOf(err), detailsOf(err)))
		}
		return
	}
	if max := s.card.Limits.MaxBody; max > 0 && int64(len(f.Body)) > max {
		if f.Type == TypeCall {
			_ = c.send(NewErrorReply(f, s.card.Actor, CodeResourceExhausted, "body 超过 max_body", false, nil))
		} else {
			s.metrics.Counter("sump_dropped_total", "Dropped frames", map[string]string{"reason": "max_body"}, 1)
		}
		return
	}
	switch f.Type {
	case TypeCall:
		h, ok := s.handlers[f.Method]
		if !ok {
			_ = c.send(NewErrorReply(f, s.card.Actor, CodeNotFound, "unknown method: "+f.Method, false, nil))
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			_ = c.send(NewErrorReply(f, s.card.Actor, CodeResourceExhausted, "server inflight limit", true, nil))
			return
		}
		go func() {
			defer func() { <-s.sem }()
			s.runCall(c, f, h)
		}()
	case TypeEvent:
		h, ok := s.handlers[f.Method]
		if !ok {
			s.metrics.Counter("sump_unknown_event_total", "Unknown events", map[string]string{"method": f.Method}, 1)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.ActionTimeout)
			defer cancel()
			call := &Call{Frame: f, srv: s, conn: c}
			if err := h(ctx, call); err != nil {
				s.metrics.Counter("sump_event_errors_total", "Event handler errors", map[string]string{"method": f.Method}, 1)
			}
		}()
	}
}

func (s *Server) runCall(c *conn, f Frame, h Handler) {
	start := time.Now()
	idemKey := ""
	if s.idem != nil && f.IdempotencyKey != "" {
		if m, ok := s.card.Method(f.Method); ok && m.Idempotent {
			idemKey = idemKeyOf(f)
			if cached, ok := s.idem.get(idemKey); ok {
				cached.ID = NewID()
				cached.ReplyTo = f.ID
				cached.To = f.From
				_ = c.send(cached)
				code := StatusOK
				if cached.Status == StatusError {
					code = StatusError
					if cached.Error != nil && cached.Error.Code != "" {
						code = cached.Error.Code
					}
				}
				s.metrics.Counter("sump_requests_total", "Total requests", map[string]string{"method": f.Method, "code": code}, 1)
				s.metrics.Observe("sump_request_duration_seconds", "Request latency", "s", map[string]string{"method": f.Method}, time.Since(start).Seconds())
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ActionTimeout)
	defer cancel()
	call := &Call{Frame: f, srv: s, conn: c, idemKey: idemKey}
	s.metrics.AddGauge("sump_inflight_requests", "In-flight requests", map[string]string{"method": f.Method}, 1)
	err := h(ctx, call)
	if err != nil && !call.Replied() {
		_ = call.ReplyError(codeOf(err), err.Error(), retryableOf(err), detailsOf(err))
	}
	if !call.Replied() {
		_ = call.ReplyError(CodeInternal, "handler did not reply", false, nil)
	}
	s.metrics.AddGauge("sump_inflight_requests", "In-flight requests", map[string]string{"method": f.Method}, -1)
	code := StatusOK
	if call.lastError != nil {
		code = call.lastError.Code
	}
	s.metrics.Counter("sump_requests_total", "Total requests", map[string]string{"method": f.Method, "code": code}, 1)
	s.metrics.Observe("sump_request_duration_seconds", "Request latency", "s", map[string]string{"method": f.Method}, time.Since(start).Seconds())
}

func codeOf(err error) string {
	if e, ok := err.(*Error); ok && e.Code != "" {
		return e.Code
	}
	return CodeInternal
}
func retryableOf(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.Retryable
	}
	return false
}
func detailsOf(err error) map[string]any {
	if e, ok := err.(*Error); ok {
		return e.Details
	}
	return nil
}

type conn struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	closed bool
}

func (c *conn) send(f Frame) error {
	data, err := Marshal(f)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return ErrConnClosed
	}
	return c.ws.WriteMessage(websocket.BinaryMessage, data)
}

func (c *conn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.ws.Close()
}

// Call 是一次入站调用。
type Call struct {
	Frame Frame
	srv   *Server
	conn  *conn

	mu        sync.Mutex
	replied   bool
	lastError *Error
	idemKey   string
}

// Replied 是否已经回应。
func (c *Call) Replied() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.replied
}

// Reply 发送成功响应。
func (c *Call) Reply(body any) error {
	f, err := NewReply(c.Frame, c.srv.card.Actor, body)
	if err != nil {
		return err
	}
	c.mu.Lock()
	if c.replied {
		c.mu.Unlock()
		return ErrCode(CodeConflict, "call already replied", false, nil)
	}
	c.replied = true
	c.mu.Unlock()
	err = c.conn.send(f)
	if err == nil && c.idemKey != "" && c.srv.idem != nil {
		c.srv.idem.put(c.idemKey, f)
	}
	return err
}

// ReplyError 发送失败响应。
func (c *Call) ReplyError(code, msg string, retryable bool, details map[string]any) error {
	f := NewErrorReply(c.Frame, c.srv.card.Actor, code, msg, retryable, details)
	c.mu.Lock()
	if c.replied {
		c.mu.Unlock()
		return ErrCode(CodeConflict, "call already replied", false, nil)
	}
	c.replied = true
	c.lastError = f.Error
	c.mu.Unlock()
	err := c.conn.send(f)
	if err == nil && c.idemKey != "" && c.srv.idem != nil {
		c.srv.idem.put(c.idemKey, f)
	}
	return err
}

// SendEvent 向调用方发事件。
func (c *Call) SendEvent(method string, body any) error {
	f, err := NewEvent(c.Frame.Work, c.srv.card.Actor, c.Frame.From, method, body)
	if err != nil {
		return err
	}
	return c.conn.send(f)
}

// SendStream 发送流式块；end=true 表示终态。
func (c *Call) SendStream(streamID string, seq int64, end bool, body any, status string) error {
	f, err := NewStreamFrame(c.Frame, c.srv.card.Actor, streamID, seq, end, body, status)
	if err != nil {
		return err
	}
	if end {
		// 终态错误流走 SendStreamError；NewStreamFrame 不允许 status=error 且无 error。
		c.mu.Lock()
		c.replied = true
		c.mu.Unlock()
	}
	return c.conn.send(f)
}

// SendStreamError 发送错误终态的流式块。
func (c *Call) SendStreamError(streamID string, seq int64, e *Error) error {
	f, err := NewErrorStreamFrame(c.Frame, c.srv.card.Actor, streamID, seq, e)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.replied = true
	c.lastError = e
	c.mu.Unlock()
	return c.conn.send(f)
}

// Close 关闭底层连接（设置中心等长连接管理用）。
func (c *Call) Close() error { return c.conn.Close() }

// WorkID 返回 work id。
func (c *Call) WorkID() string { return c.Frame.Work }

type HelloRequest struct {
	Protocol     string   `msgpack:"protocol" json:"protocol"`
	Actor        Ref      `msgpack:"actor" json:"actor"`
	Capabilities []string `msgpack:"capabilities,omitempty" json:"capabilities,omitempty"`
	Auth         any      `msgpack:"auth,omitempty" json:"auth,omitempty"`
}

type HelloResponse struct {
	Card Card `msgpack:"card" json:"card"`
}

type HealthCheck struct {
	Name      string `msgpack:"name" json:"name"`
	Status    string `msgpack:"status" json:"status"`
	LatencyMS int64  `msgpack:"latency_ms,omitempty" json:"latency_ms,omitempty"`
}

type HealthResponse struct {
	Status   string        `msgpack:"status" json:"status"`
	Version  string        `msgpack:"version" json:"version"`
	UptimeMS int64         `msgpack:"uptime_ms" json:"uptime_ms"`
	Checks   []HealthCheck `msgpack:"checks,omitempty" json:"checks,omitempty"`
}

type MetricsResponse struct {
	Service   string   `msgpack:"service" json:"service"`
	Version   string   `msgpack:"version" json:"version"`
	Timestamp string   `msgpack:"timestamp" json:"timestamp"`
	Metrics   []Metric `msgpack:"metrics" json:"metrics"`
}

func (s *Server) handleHello(_ context.Context, c *Call) error {
	var req HelloRequest
	if err := c.Frame.DecodeBody(&req); err != nil {
		return ErrCode(CodeInvalidArgument, "hello body 无效: "+err.Error(), false, nil)
	}
	if req.Protocol != "" && req.Protocol != ProtocolVersion {
		return ErrCode(CodeInvalidArgument, "protocol 不匹配: "+req.Protocol, false, nil)
	}
	return c.Reply(HelloResponse{Card: s.card})
}

func (s *Server) handleHealth(_ context.Context, c *Call) error {
	return c.Reply(HealthResponse{Status: "ok", Version: s.card.Version, UptimeMS: time.Since(s.started).Milliseconds()})
}

func (s *Server) handleMetrics(_ context.Context, c *Call) error {
	return c.Reply(MetricsResponse{
		Service: s.card.Name, Version: s.card.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339), Metrics: s.metrics.Snapshot(),
	})
}
