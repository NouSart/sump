package spv2

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Handler 处理一次 call/event。call 必须通过 Call.Reply / ReplyError / SendStream 回应。
type Handler func(ctx context.Context, call *Call) error

// 丢弃原因（sump_dropped_total 的 reason 标签）：一类事件一个取值，全部低基数、可枚举。
const (
	dropUndecodable  = "undecodable"     // 反序列化失败：连帧头都拿不到，无法归属
	dropInvalidFrame = "invalid_frame"   // 帧校验失败：版本 / 必填字段 / 目标不是本服务
	dropMaxBody      = "max_body"        // body 超过服务卡上限
	dropMaxHeader    = "max_header"      // 帧头超过上限
	dropNoDispatcher = "unhandled_frame" // 校验通过但没有分发分支（如 reply/stream 帧打到服务端）
	dropNoHello      = "no_hello"        // 连接首帧不是 hello（spec §5.1 握手未完成）
)

// ServerOptions 是服务端选项。
type ServerOptions struct {
	MaxMessageBytes int64
	ActionTimeout   time.Duration
	MaxInflight     int
	Metrics         *Registry
	Idempotency     *IdempotencyCache
	Logger          *log.Logger // 丢弃与 panic 的落点；默认 log.Default()
	HealthChecks    []HealthChecker
	// HealthTimeout 是 health method 与 /readyz 执行探针的总上限（默认 5s）。
	// 探针必须尊重 ctx；超时尚未返回的探针按 fail 计，健康端点不跟着挂住。
	HealthTimeout time.Duration
	// MaxStreams 是单连接同时打开的流上限（spec §3.5 默认 32）；<=0 时取卡片 max_stream。
	MaxStreams int
	// MaxStreamBuffer 是单流累计发送字节上限（spec §3.5 默认 1 MiB）。
	MaxStreamBuffer int64
	// MaxHeader 是帧头上限（spec §3.5 默认 16 KiB）；<=0 时取卡片 max_header。
	MaxHeader int64
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

// WithLogger 指定日志落点：丢弃帧与处理方 panic 必须留下痕迹，不能静默。
func WithLogger(l *log.Logger) ServerOption {
	return func(o *ServerOptions) { o.Logger = l }
}

// WithHealthChecks 注入依赖探针；health method 与 /readyz 使用同一组探针。
func WithHealthChecks(checkers ...HealthChecker) ServerOption {
	return func(o *ServerOptions) { o.HealthChecks = append(o.HealthChecks, checkers...) }
}

// WithHealthTimeout 指定健康探针的总执行上限（<=0 表示沿用调用方 ctx 的 deadline）。
func WithHealthTimeout(d time.Duration) ServerOption {
	return func(o *ServerOptions) { o.HealthTimeout = d }
}

// WithMaxStreams 覆盖单连接同时打开的流上限（默认取卡片 max_stream，缺省 32）。
func WithMaxStreams(n int) ServerOption {
	return func(o *ServerOptions) { o.MaxStreams = n }
}

// WithMaxStreamBuffer 覆盖单流累计发送字节上限（默认 1 MiB）。
func WithMaxStreamBuffer(n int64) ServerOption {
	return func(o *ServerOptions) { o.MaxStreamBuffer = n }
}

// WithMaxHeader 覆盖帧头上限（默认取卡片 max_header，缺省 16 KiB）。
func WithMaxHeader(n int64) ServerOption {
	return func(o *ServerOptions) { o.MaxHeader = n }
}

// Server 是一个 SPv2.1 服务端。
type Server struct {
	card     Card
	handlers map[string]Handler
	opts     ServerOptions
	metrics  *Registry
	idem     *IdempotencyCache
	logger   *log.Logger
	sem      chan struct{}
	upgrader websocket.Upgrader
	started  time.Time
}

// NewServer 创建服务端。
func NewServer(card Card, opts ...ServerOption) (*Server, error) {
	if err := card.Validate(); err != nil {
		return nil, err
	}
	o := ServerOptions{MaxMessageBytes: 4 << 20, ActionTimeout: 30 * time.Second, MaxInflight: 64, HealthTimeout: 5 * time.Second, MaxStreamBuffer: 1 << 20}
	for _, opt := range opts {
		opt(&o)
	}
	for i, checker := range o.HealthChecks {
		if checker == nil {
			return nil, fmt.Errorf("health 探针 %d 为 nil", i)
		}
	}
	if o.Metrics == nil {
		o.Metrics = NewRegistry()
	}
	if o.Logger == nil {
		o.Logger = log.Default()
	}
	if o.MaxStreams <= 0 {
		if card.Limits.MaxStream > 0 {
			o.MaxStreams = card.Limits.MaxStream
		} else {
			o.MaxStreams = 32
		}
	}
	if o.MaxStreamBuffer <= 0 {
		o.MaxStreamBuffer = 1 << 20
	}
	if o.MaxHeader <= 0 {
		if card.Limits.MaxHeader > 0 {
			o.MaxHeader = card.Limits.MaxHeader
		} else {
			o.MaxHeader = 16 << 10
		}
	}
	if card.Limits.MaxHeader <= 0 {
		card.Limits.MaxHeader = o.MaxHeader
	}
	s := &Server{
		card: card, handlers: map[string]Handler{}, opts: o, metrics: o.Metrics, idem: o.Idempotency,
		logger: o.Logger,
		sem:    make(chan struct{}, o.MaxInflight), started: time.Now(),
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

// Handler 返回 HTTP handler（挂到 /spv2）。
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
			// 解不开就不知道帧头（work / id / type 全没有），既无法回复也无法继续解析这条流；
			// 结束连接是让调用方立刻拿到 ErrConnClosed 的唯一办法。但"结束"不等于"不留痕迹"：
			// 丢弃必须计数 + 落日志，否则这条链路上谁也看不出帧被吃掉了。
			s.countDropped(Frame{}, dropUndecodable, err.Error())
			return
		}
		s.dispatch(c, f)
	}
}

func (s *Server) dispatch(c *conn, f Frame) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		s.reject(c, f, dropInvalidFrame, codeOf(err), err.Error(), retryableOf(err), detailsOf(err))
		return
	}
	if err := ValidateTo(f, s.card.Actor); err != nil {
		s.reject(c, f, dropInvalidFrame, codeOf(err), err.Error(), retryableOf(err), detailsOf(err))
		return
	}
	if max := s.card.Limits.MaxBody; max > 0 && int64(len(f.Body)) > max {
		s.reject(c, f, dropMaxBody, CodeResourceExhausted, "body 超过 max_body", true, nil)
		return
	}
	// spec §3.5 max_header：帧头（不含 body）超限同样拒绝，不能等解完大 body 才说。
	if max := s.opts.MaxHeader; max > 0 {
		headerOnly := f
		headerOnly.Body = nil
		if raw, err := Marshal(headerOnly); err == nil && int64(len(raw)) > max {
			s.reject(c, f, dropMaxHeader, CodeResourceExhausted, "header 超过 max_header", false, nil)
			return
		}
	}
	// spec §5.1：连接后第一帧必须是 call method=hello；在此之前其它帧一律拒绝，
	// 不能默认信任一个没握手的对端（也不静默丢弃：call 要回错误，其它帧计数+日志）。
	if !c.helloOK() && !(f.Type == TypeCall && f.Method == "hello") {
		s.reject(c, f, dropNoHello, CodeInvalidArgument, "连接首帧必须是 hello", false, nil)
		return
	}
	// spec §10：流的取消是 call method=cancel 带 stream（可选 work）。
	// 这里只拦"带 stream 的 cancel"这一种传输层取消；服务自己的 cancel（work 取消）照常进 handler。
	if f.Type == TypeCall && f.Method == "cancel" {
		var req struct {
			Stream string `msgpack:"stream,omitempty"`
		}
		_ = f.DecodeBody(&req)
		if req.Stream != "" {
			n := c.cancelStream(req.Stream)
			reply, err := NewReply(f, s.card.Actor, map[string]any{"cancelled": n})
			if err != nil {
				return
			}
			s.metrics.Counter("sump_stream_cancelled_total", "Stream cancellations", map[string]string{"outcome": "transport"}, float64(n))
			_ = c.send(reply)
			return
		}
	}
	switch f.Type {
	case TypeCall:
		s.observeRequestBytes(f.Method, len(f.Body))
		if notDesignedMethods[f.Method] {
			reply := NewErrorReply(f, s.card.Actor, CodeNotSupported, f.Method+" 在 v2.1 未设计", false, nil)
			s.countError(CodeNotSupported)
			s.observeResponseBytes(f.Method, len(reply.Body))
			_ = c.send(reply)
			return
		}
		h, ok := s.handlers[f.Method]
		if !ok {
			// 未知 method 的 call 必须回错误 reply（spec §8），这一帧有归属、有回执，不是丢弃。
			reply := NewErrorReply(f, s.card.Actor, CodeNotFound, "unknown method: "+f.Method, false, nil)
			s.countError(CodeNotFound)
			s.observeResponseBytes(f.Method, len(reply.Body))
			_ = c.send(reply)
			return
		}
		select {
		case s.sem <- struct{}{}:
		default:
			reply := NewErrorReply(f, s.card.Actor, CodeResourceExhausted, "server inflight limit", true, nil)
			s.countError(CodeResourceExhausted)
			s.observeResponseBytes(f.Method, len(reply.Body))
			_ = c.send(reply)
			return
		}
		go func() {
			defer func() { <-s.sem }()
			s.runCall(c, f, h)
		}()
	case TypeEvent:
		s.observeRequestBytes(f.Method, len(f.Body))
		h, ok := s.handlers[f.Method]
		if !ok {
			s.metrics.Counter("sump_unknown_event_total", "Unknown events", map[string]string{"method": s.methodLabel(f.Method)}, 1)
			return
		}
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.opts.ActionTimeout)
			defer cancel()
			call := &Call{Frame: f, srv: s, conn: c}
			err, panicked := s.invoke(ctx, call, h)
			if panicked {
				return // panic 已经进 sump_panics_total；同一件事不记两处
			}
			if err != nil {
				s.metrics.Counter("sump_event_errors_total", "Event handler errors", map[string]string{"method": s.methodLabel(f.Method)}, 1)
				s.countError(codeOf(err))
			}
		}()
	default:
		// 校验通过但这里没有分支可走（服务端不该收到 reply/stream 帧）：过去静默 return。
		s.countDropped(f, dropNoDispatcher, "")
	}
}

// reject 是「入站帧没通过检查」的唯一处理口径：先按同一事件记账，
// 再对 call 补一帧错误 reply——补回执只为不让调用方空等，不改变事件的归类。
// 校验失败对 call / event / 其它帧一视同仁：一个事件，一个指标，一个 reason。
func (s *Server) reject(c *conn, f Frame, reason, code, msg string, retryable bool, details map[string]any) {
	s.countDropped(f, reason, msg)
	if f.Type == TypeCall {
		s.countError(code)
		reply := NewErrorReply(f, s.card.Actor, code, msg, retryable, details)
		s.observeResponseBytes(f.Method, len(reply.Body))
		_ = c.send(reply)
	}
}

// countDropped 累加 sump_dropped_total 并留一条日志（指标是聚合，日志是单条）。
// detail 只放原因性的短文本；不打 body 全文，也不把原始字节写进标签。
func (s *Server) countDropped(f Frame, reason, detail string) {
	// 先落日志再计数：测试与排障都以可见痕迹为准（反过来的顺序会产生
	// 「指标已到、日志未到」的竞态窗口，见 server_isolation_test.go）。
	msg := fmt.Sprintf("丢弃入站帧 reason=%s type=%q method=%q work=%q id=%q", reason, f.Type, f.Method, f.Work, f.ID)
	if detail != "" {
		msg += "：" + detail
	}
	s.logger.Printf("%s", msg)
	s.metrics.Counter("sump_dropped_total", "Dropped frames", map[string]string{"reason": reason}, 1)
}

// countPanic 累加 sump_panics_total 并把 panic 值与栈写进日志。
// 栈只进日志：指标标签必须低基数，错误信息也不该把内部细节带上线。
func (s *Server) countPanic(f Frame, r any) {
	s.metrics.Counter("sump_panics_total", "Recovered handler panics", map[string]string{"method": s.methodLabel(f.Method)}, 1)
	s.logger.Printf("处理方 panic 已隔离 method=%q work=%q id=%q: %v\n%s", f.Method, f.Work, f.ID, r, debug.Stack())
}

// 传输域最小指标（spec §12.3）：请求/响应字节与错误码。标签只放低基数的 method/code。
func (s *Server) observeRequestBytes(method string, n int) {
	s.metrics.Observe("sump_request_bytes", "Request body bytes", "bytes", map[string]string{"method": s.methodLabel(method)}, float64(n))
}

func (s *Server) observeResponseBytes(method string, n int) {
	s.metrics.Observe("sump_response_bytes", "Response body bytes", "bytes", map[string]string{"method": s.methodLabel(method)}, float64(n))
}

func (s *Server) countError(code string) {
	s.metrics.Counter("sump_errors_total", "Error replies", map[string]string{"code": code}, 1)
}

// methodLabel 把对端可控的 method 字符串收敛到卡片声明过的名字；
// 未声明的统一记 "other"，避免未知 method 把指标序列打爆（spec §12.2 低基数）。
func (s *Server) methodLabel(method string) string {
	if _, ok := s.card.Method(method); ok {
		return method
	}
	// 运行时注册的 handler 也是有限集合（代码里写死的），可以保留；只有对端随便起的
	// 未知名字才收敛到 other，避免指标序列被打爆。
	if _, ok := s.handlers[method]; ok {
		return method
	}
	return "other"
}

// invoke 跑 handler，并把 panic 关在这一次调用里：转成 internal 错误交回调用方。
// 不 recover 的话，一个 goroutine 里的 panic 会掀掉整个进程，同一条链上别的在途请求一起没。
// panicked 单独返回，是为了让调用方区分"处理方返回了错误"和"处理方炸了"，不重复计数。
func (s *Server) invoke(ctx context.Context, call *Call, h Handler) (err error, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			panicked = true
			s.countPanic(call.Frame, r)
			err = ErrCode(CodeInternal, fmt.Sprintf("处理方 %s 发生 panic，已隔离", call.Frame.Method), true, nil)
		}
	}()
	return h(ctx, call), false
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
				code := StatusOK
				if cached.Status == StatusError {
					code = StatusError
					if cached.Error != nil && cached.Error.Code != "" {
						code = cached.Error.Code
					}
				}
				if code != StatusOK {
					s.countError(code)
				}
				s.observeResponseBytes(f.Method, len(cached.Body))
				_ = c.send(cached)
				s.metrics.Counter("sump_requests_total", "Total requests", map[string]string{"method": s.methodLabel(f.Method), "code": code}, 1)
				s.metrics.Observe("sump_request_duration_seconds", "Request latency", "s", map[string]string{"method": s.methodLabel(f.Method)}, time.Since(start).Seconds())
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), s.opts.ActionTimeout)
	defer cancel()
	call := &Call{Frame: f, srv: s, conn: c, idemKey: idemKey}
	s.metrics.AddGauge("sump_inflight_requests", "In-flight requests", map[string]string{"method": s.methodLabel(f.Method)}, 1)
	// panic 也走这条错误路径：invoke 把它转成 internal，调用方拿到回执而不是空等到超时。
	// handler 在独立 goroutine 里跑，到期先回 deadline_exceeded（spec §9.3「到点必须回
	// deadline_exceeded；不得挂住」）。handler 之后才返回的结果按 late result 丢弃：
	// Call.Reply / ReplyError / SendStream 已回应过一律拒绝。
	type runResult struct{ err error }
	done := make(chan runResult, 1)
	go func() {
		err, _ := s.invoke(ctx, call, h)
		done <- runResult{err: err}
	}()
	var err error
	select {
	case res := <-done:
		err = res.err
	case <-ctx.Done():
		_ = call.ReplyError(CodeDeadlineExceeded, "处理方超过 action timeout，未在期限内返回", true, map[string]any{
			"method": f.Method, "timeout_ms": s.opts.ActionTimeout.Milliseconds(),
		})
	}
	if err != nil && !call.Replied() {
		_ = call.ReplyError(codeOf(err), err.Error(), retryableOf(err), detailsOf(err))
	}
	if !call.Replied() {
		_ = call.ReplyError(CodeInternal, "handler did not reply", true, nil)
	}
	s.metrics.AddGauge("sump_inflight_requests", "In-flight requests", map[string]string{"method": s.methodLabel(f.Method)}, -1)
	code := StatusOK
	if call.lastError != nil {
		code = call.lastError.Code
	}
	s.metrics.Counter("sump_requests_total", "Total requests", map[string]string{"method": s.methodLabel(f.Method), "code": code}, 1)
	s.metrics.Observe("sump_request_duration_seconds", "Request latency", "s", map[string]string{"method": s.methodLabel(f.Method)}, time.Since(start).Seconds())
}

func codeOf(err error) string {
	if e, ok := err.(*Error); ok && e.Code != "" {
		return e.Code
	}
	// spec §8：调用方取消是 cancelled，不是 internal；handler 自己返回 ctx 超时同理。
	switch {
	case errors.Is(err, context.Canceled):
		return CodeCancelled
	case errors.Is(err, context.DeadlineExceeded):
		return CodeDeadlineExceeded
	}
	return CodeInternal
}

func retryableOf(err error) bool {
	if e, ok := err.(*Error); ok {
		return e.Retryable
	}
	return RetryableByCode(codeOf(err))
}
func detailsOf(err error) map[string]any {
	if e, ok := err.(*Error); ok {
		return e.Details
	}
	return nil
}

type conn struct {
	ws        *websocket.Conn
	mu        sync.Mutex
	closed    bool
	helloDone bool
	// streams 是「本连接上由服务端发出的流」的账本：key = reply_to|stream_id。
	// 只用于执行 spec §3.5/§10 的并发与单流字节上限，不承载业务状态。
	streams map[string]*outStream
}

type outStream struct {
	bytes   int64
	lastSeq int64
}

// trackStream 执行服务端发送侧的流契约：seq 从 1 递增、并发流不超上限、
// 单流累计字节不超上限。end=true 时释放该流。
func (c *conn) trackStream(replyTo, streamID string, seq int64, size int, end bool, maxStreams int, maxBuffer int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.streams == nil {
		c.streams = map[string]*outStream{}
	}
	key := replyTo + "|" + streamID
	st := c.streams[key]
	if st == nil {
		if seq != 1 {
			return ErrCode(CodeInvalidArgument, "stream.seq 必须从 1 开始", false, nil)
		}
		if maxStreams > 0 && len(c.streams) >= maxStreams {
			return ErrCode(CodeResourceExhausted, "同时打开的流超过 max_stream", true, map[string]any{"max_stream": maxStreams})
		}
		st = &outStream{lastSeq: 0}
		c.streams[key] = st
	} else if seq != st.lastSeq+1 {
		return ErrCode(CodeInvalidArgument, "stream.seq 必须逐 1 递增", false, map[string]any{"last_seq": st.lastSeq, "seq": seq})
	}
	st.lastSeq = seq
	st.bytes += int64(size)
	if maxBuffer > 0 && st.bytes > maxBuffer {
		delete(c.streams, key)
		return ErrCode(CodeResourceExhausted, "流超过 max_stream_buffer", true, map[string]any{"max_stream_buffer": maxBuffer})
	}
	if end {
		delete(c.streams, key)
	}
	return nil
}

// cancelStream 取消本连接上指定 stream id 的所有账本条目，返回释放数量。
// 这是传输层 best-effort：在飞 handler 可能还会写一帧，但账本已释放，后续 seq 不连续会被拒。
func (c *conn) cancelStream(streamID string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	suffix := "|" + streamID
	for key := range c.streams {
		if strings.HasSuffix(key, suffix) {
			delete(c.streams, key)
			n++
		}
	}
	return n
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

// markHello 记录本连接已经完成 §5.1 握手。
func (c *conn) markHello() {
	c.mu.Lock()
	c.helloDone = true
	c.mu.Unlock()
}

func (c *conn) helloOK() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.helloDone
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
	c.srv.observeResponseBytes(c.Frame.Method, len(f.Body))
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
	c.srv.countError(code)
	c.srv.observeResponseBytes(c.Frame.Method, len(f.Body))
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
	if c.Replied() {
		// 已回错误终态（如 deadline_exceeded）之后，流式块属于 late result，必须丢弃，
		// 不能再往线上发第二个终态。
		return ErrCode(CodeConflict, "call already replied", false, nil)
	}
	if err := c.conn.trackStream(c.Frame.ID, streamID, seq, len(f.Body), end, c.srv.opts.MaxStreams, c.srv.opts.MaxStreamBuffer); err != nil {
		return err
	}
	if end {
		// 终态错误流走 SendStreamError；NewStreamFrame 不允许 status=error 且无 error。
		c.mu.Lock()
		if c.replied {
			c.mu.Unlock()
			return ErrCode(CodeConflict, "call already replied", false, nil)
		}
		c.replied = true
		c.mu.Unlock()
	}
	c.srv.observeResponseBytes(c.Frame.Method, len(f.Body))
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
	c.srv.countError(e.Code)
	c.srv.observeResponseBytes(c.Frame.Method, len(f.Body))
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
	LatencyMS int64  `msgpack:"latency_ms" json:"latency_ms"`
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
	if c.conn != nil {
		c.conn.markHello()
	}
	return c.Reply(HelloResponse{Card: s.card})
}

func (s *Server) handleHealth(ctx context.Context, c *Call) error {
	checks, status := s.healthChecks(ctx)
	return c.Reply(HealthResponse{Status: status, Version: s.card.Version, UptimeMS: time.Since(s.started).Milliseconds(), Checks: checks})
}

func (s *Server) handleMetrics(_ context.Context, c *Call) error {
	return c.Reply(MetricsResponse{
		Service: s.card.Name, Version: s.card.Version,
		Timestamp: time.Now().UTC().Format(time.RFC3339), Metrics: s.metrics.Snapshot(),
	})
}
