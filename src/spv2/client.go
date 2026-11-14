package spv2

import (
	"context"
	"errors"
	"net"
	"sync"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

// ErrConnClosed 表示连接关闭。
var ErrConnClosed = errors.New("spv2: connection closed")

// ClientOptions 是客户端选项。
type ClientOptions struct {
	MaxMessageBytes int64
	WriteTimeout    time.Duration
	OnEvent         func(Frame)
	// Metrics 用于接收侧记账（spec §10：流 seq 缺口记 sump_stream_gap_total）。
	Metrics *Registry
}

// ClientOption 修改 ClientOptions。
type ClientOption func(*ClientOptions)

// WithClientMaxMessageBytes 设置接收上限。
func WithClientMaxMessageBytes(n int64) ClientOption {
	return func(o *ClientOptions) { o.MaxMessageBytes = n }
}

// WithClientWriteTimeout 设置写超时。
func WithClientWriteTimeout(d time.Duration) ClientOption {
	return func(o *ClientOptions) { o.WriteTimeout = d }
}

// WithOnEvent 设置事件回调。
func WithOnEvent(fn func(Frame)) ClientOption {
	return func(o *ClientOptions) { o.OnEvent = fn }
}

// WithClientMetrics 让客户端把接收侧异常（流 seq 缺口/重复）记进指标。
func WithClientMetrics(r *Registry) ClientOption {
	return func(o *ClientOptions) { o.Metrics = r }
}

// Client 是一条 SPv2.1 WS 连接。
type Client struct {
	conn         *websocket.Conn
	writeMu      sync.Mutex
	writeTimeout time.Duration

	pendingMu sync.Mutex
	pending   map[string]chan Frame

	// streams 的读/删/关闭统一由 streamMu 保护：CancelStream 与 readLoop 不能
	// 一个在 close 一个在 send（否则 panic）。streamIDs 记 stream.id → call id。
	streamMu  sync.Mutex
	streams   map[string]chan Frame
	streamIDs map[string]string
	// streamSeq 记录每个 (reply_to, stream.id) 已收到的最大 seq，用于缺口/重复判定。
	streamSeq map[string]int64

	// §5.1：连接的第一帧必须是 hello。客户端在第一次真正收发前自动完成握手，
	// 目标取该帧的 to（调用方本来就知道要访问哪个服务）。
	hsMu        sync.Mutex
	hsPerformMu sync.Mutex
	handshook   bool
	hsErr       error

	metrics *Registry
	onEvent func(Frame)
	closed  chan struct{}
	once    sync.Once
}

// Dial 建立连接。
func Dial(ctx context.Context, url string, opts ...ClientOption) (*Client, error) {
	o := ClientOptions{MaxMessageBytes: 4 << 20, WriteTimeout: 5 * time.Second}
	for _, opt := range opts {
		opt(&o)
	}
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = 5 * time.Second
	conn, _, err := d.DialContext(ctx, url, nil)
	if err != nil {
		// spec §8：目标不可达是 unavailable（retryable=true），不是裸网络错误。
		return nil, ErrCode(CodeUnavailable, "连接 "+url+" 失败: "+err.Error(), true, nil)
	}
	if o.MaxMessageBytes > 0 {
		conn.SetReadLimit(o.MaxMessageBytes)
	}
	c := &Client{
		conn: conn, writeTimeout: o.WriteTimeout,
		pending: map[string]chan Frame{}, streams: map[string]chan Frame{},
		streamIDs: map[string]string{},
		streamSeq: map[string]int64{}, metrics: o.Metrics,
		onEvent: o.OnEvent, closed: make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Close 关闭连接。
func (c *Client) Close() error {
	c.once.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
	return nil
}

// Done 返回关闭信号。
func (c *Client) Done() <-chan struct{} { return c.closed }

func (c *Client) send(f Frame) error {
	f.Normalize()
	if err := f.Validate(); err != nil {
		return err
	}
	data, err := Marshal(f)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.writeTimeout > 0 {
		_ = c.conn.SetWriteDeadline(time.Now().Add(c.writeTimeout))
	}
	if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		// 对端已关闭的几种表现统一成 ErrConnClosed，调用方只需处理一种关闭错误：
		// ErrCloseSent（gorilla 已知关闭）、net.ErrClosed、以及 RST/EPIPE（对端已消失）。
		if connGone(err) {
			_ = c.Close()
			return ErrConnClosed
		}
		return err
	}
	return nil
}

// Send 发送一帧（事件/不等待回复的 call）。首帧前自动完成 §5.1 握手。
func (c *Client) Send(f Frame) error {
	if err := c.ensureHandshake(f.From, f.To); err != nil {
		return err
	}
	return c.send(f)
}

// Call 发送 call 并等待 reply。首帧前自动完成 §5.1 握手。
func (c *Client) Call(ctx context.Context, f Frame) (Frame, error) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	if err := c.ensureHandshake(f.From, f.To); err != nil {
		return Frame{}, err
	}
	return c.callRaw(ctx, f, f.ID)
}

// callRaw 是 Call 的发送/等待半段（不含握手），握手自己也用它。
func (c *Client) callRaw(ctx context.Context, f Frame, id string) (Frame, error) {
	ch := make(chan Frame, 1)
	c.pendingMu.Lock()
	c.pending[id] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, id)
		c.pendingMu.Unlock()
	}()
	if err := c.send(f); err != nil {
		return Frame{}, err
	}
	select {
	case resp := <-ch:
		if resp.Status == StatusError {
			if resp.Error != nil {
				return resp, resp.Error
			}
			return resp, errors.New("spv2: error reply without error")
		}
		return resp, nil
	case <-ctx.Done():
		return Frame{}, ctx.Err()
	case <-c.closed:
		return Frame{}, ErrConnClosed
	}
}

// CancelStream 取消一条正在接收的流（spec §10：call method=cancel 带 stream）。
// 返回服务端释放的流数量（1 = 确实取消了；0 = 账本里已没有这条流）。
// 取消后本地流通道会被关闭，调用方的 range 能正常退出。
func (c *Client) CancelStream(ctx context.Context, from, to Ref, work, streamID string) (int, error) {
	call, err := NewCall(work, from, to, "cancel", map[string]any{"stream": streamID})
	if err != nil {
		return 0, err
	}
	resp, err := c.Call(ctx, call)
	if err != nil {
		return 0, err
	}
	var out struct {
		Cancelled int `msgpack:"cancelled"`
	}
	if err := resp.DecodeBody(&out); err != nil {
		return 0, err
	}
	c.streamMu.Lock()
	callID := c.streamIDs[streamID]
	if callID != "" {
		delete(c.streamIDs, streamID)
		if ch := c.streams[callID]; ch != nil {
			delete(c.streams, callID)
			close(ch)
		}
	}
	c.streamMu.Unlock()
	return out.Cancelled, nil
}

// CallDecode 发送 call，成功时把 body 解码到 dst。
func (c *Client) CallDecode(ctx context.Context, f Frame, dst any) error {
	resp, err := c.Call(ctx, f)
	if err != nil {
		return err
	}
	return resp.DecodeBody(dst)
}

// Stream 发送 call，返回流式帧通道。end=true 的帧到达后关闭。首帧前自动握手。
func (c *Client) Stream(ctx context.Context, f Frame) (<-chan Frame, error) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if err := c.ensureHandshake(f.From, f.To); err != nil {
		return nil, err
	}
	ch := make(chan Frame, 16)
	c.streamMu.Lock()
	c.streams[f.ID] = ch
	c.streamMu.Unlock()
	if err := c.send(f); err != nil {
		c.streamMu.Lock()
		delete(c.streams, f.ID)
		c.streamMu.Unlock()
		return nil, err
	}
	out := make(chan Frame)
	go func() {
		defer close(out)
		for {
			select {
			case fr, ok := <-ch:
				if !ok {
					return
				}
				out <- fr
				if fr.Type == TypeReply || (fr.Stream != nil && fr.Stream.End) {
					return
				}
			case <-ctx.Done():
				return
			case <-c.closed:
				return
			}
		}
	}()
	return out, nil
}

// ensureHandshake 在第一次收发前发一条 hello（spec §5.1），并校验服务卡。
// 目标服务名来自调用帧的 to：调用方本来就知道自己访问谁，不需要额外配置。
func (c *Client) ensureHandshake(from, to Ref) error {
	c.hsMu.Lock()
	if c.handshook {
		c.hsMu.Unlock()
		return nil
	}
	if c.hsErr != nil {
		err := c.hsErr
		c.hsMu.Unlock()
		return err
	}
	c.hsMu.Unlock()

	c.hsPerformMu.Lock()
	defer c.hsPerformMu.Unlock()
	c.hsMu.Lock()
	if c.handshook {
		c.hsMu.Unlock()
		return nil
	}
	if c.hsErr != nil {
		err := c.hsErr
		c.hsMu.Unlock()
		return err
	}
	c.hsMu.Unlock()

	fail := func(err error) error {
		c.hsMu.Lock()
		c.hsErr = err
		c.hsMu.Unlock()
		return err
	}
	if to.Kind != "service" || to.ID == "" {
		return fail(ErrCode(CodeInvalidArgument, "握手目标必须是具名 service（to.kind=service 且 to.id 非空）", false, nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	f, err := NewCall(NewID(), from, to, "hello", map[string]any{
		"protocol":     ProtocolVersion,
		"actor":        from,
		"capabilities": []string{"msgpack", "json", "stream"},
	})
	if err != nil {
		return fail(err)
	}
	resp, err := c.callRaw(ctx, f, f.ID)
	if err != nil {
		return fail(err)
	}
	var hr HelloResponse
	if err := resp.DecodeBody(&hr); err != nil {
		return fail(ErrCode(CodeInvalidArgument, "hello 回执不是服务卡: "+err.Error(), false, nil))
	}
	if hr.Card.Protocol != ProtocolVersion {
		return fail(ErrCode(CodeInvalidArgument, "对端协议不是 "+ProtocolVersion, false, nil))
	}
	if hr.Card.Name != to.ID {
		return fail(ErrCode(CodeInvalidArgument, "对端服务名 "+hr.Card.Name+" 与目标 "+to.ID+" 不符", false, nil))
	}
	c.hsMu.Lock()
	c.handshook = true
	c.hsMu.Unlock()
	return nil
}

// Hello 握手并取服务卡。
func (c *Client) Hello(ctx context.Context, from, to Ref) (Card, error) {
	f, err := NewCall("handshake", from, to, "hello", map[string]any{
		"protocol":     ProtocolVersion,
		"actor":        from,
		"capabilities": []string{"msgpack", "json", "stream"},
	})
	if err != nil {
		return Card{}, err
	}
	var resp HelloResponse
	if err := c.CallDecode(ctx, f, &resp); err != nil {
		return Card{}, err
	}
	return resp.Card, nil
}

func (c *Client) readLoop() {
	defer c.Close()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		f, err := Unmarshal(data)
		if err != nil {
			return
		}
		switch f.Type {
		case TypeReply:
			c.pendingMu.Lock()
			ch := c.pending[f.ReplyTo]
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- f
				continue
			}
			c.streamMu.Lock()
			stream := c.streams[f.ReplyTo]
			if stream != nil {
				stream <- f
				delete(c.streams, f.ReplyTo)
				if f.Stream != nil {
					delete(c.streamIDs, f.Stream.ID)
				}
				close(stream)
				c.streamMu.Unlock()
				continue
			}
			c.streamMu.Unlock()
			if c.onEvent != nil {
				c.onEvent(f)
			}
		case TypeStream:
			c.observeStreamSeq(f)
			c.streamMu.Lock()
			stream := c.streams[f.ReplyTo]
			if stream != nil {
				if f.Stream != nil {
					c.streamIDs[f.Stream.ID] = f.ReplyTo
				}
				end := f.Stream != nil && f.Stream.End
				stream <- f
				if end {
					delete(c.streams, f.ReplyTo)
					if f.Stream != nil {
						delete(c.streamIDs, f.Stream.ID)
					}
					close(stream)
				}
				c.streamMu.Unlock()
				continue
			}
			c.streamMu.Unlock()
			if c.onEvent != nil {
				c.onEvent(f)
			}
		case TypeEvent:
			if c.onEvent != nil {
				c.onEvent(f)
			}
		}
	}
}

// observeStreamSeq 记录接收侧的流 seq 连续性：跳号记 outcome=missing，
// 回退/重复记 outcome=duplicate（spec §10）。
func (c *Client) observeStreamSeq(f Frame) {
	if f.Stream == nil {
		return
	}
	key := f.ReplyTo + "|" + f.Stream.ID
	c.pendingMu.Lock()
	last := c.streamSeq[key]
	c.streamSeq[key] = f.Stream.Seq
	c.pendingMu.Unlock()
	kind := ""
	switch {
	case last == 0 && f.Stream.Seq != 1:
		kind = "missing"
	case last != 0 && f.Stream.Seq == last+1:
		kind = ""
	case last != 0 && f.Stream.Seq > last+1:
		kind = "missing"
	case last != 0 && f.Stream.Seq <= last:
		kind = "duplicate"
	}
	if kind != "" && c.metrics != nil {
		c.metrics.Counter("sump_stream_gap_total", "Stream sequence gaps", map[string]string{"outcome": kind}, 1)
	}
	if f.Stream.End {
		c.pendingMu.Lock()
		delete(c.streamSeq, key)
		c.pendingMu.Unlock()
	}
}

// HTTPToWS 把 http:// 地址转为 ws://.../spv2（测试/示例用）。
func HTTPToWS(addr string) string {
	if len(addr) >= 7 && addr[:7] == "http://" {
		return "ws://" + addr[7:] + "/spv2"
	}
	if len(addr) >= 8 && addr[:8] == "https://" {
		return "wss://" + addr[8:] + "/spv2"
	}
	return addr
}

// connGone 判断写失败是否只是"对端/本端已关闭"，而不是值得上抛的网络故障。
func connGone(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, websocket.ErrCloseSent) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE)
}
