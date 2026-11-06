package spv2

import (
	"context"
	"errors"
	"net/http"
	"sync"
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

// Client 是一条 SPv2.1 WS 连接。
type Client struct {
	conn         *websocket.Conn
	writeMu      sync.Mutex
	writeTimeout time.Duration

	pendingMu sync.Mutex
	pending   map[string]chan Frame
	streams   map[string]chan Frame

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
		return nil, err
	}
	if o.MaxMessageBytes > 0 {
		conn.SetReadLimit(o.MaxMessageBytes)
	}
	c := &Client{
		conn: conn, writeTimeout: o.WriteTimeout,
		pending: map[string]chan Frame{}, streams: map[string]chan Frame{},
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
	return c.conn.WriteMessage(websocket.BinaryMessage, data)
}

// Send 发送一帧（事件/不等待回复的 call）。
func (c *Client) Send(f Frame) error { return c.send(f) }

// Call 发送 call 并等待 reply。
func (c *Client) Call(ctx context.Context, f Frame) (Frame, error) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		return Frame{}, err
	}
	ch := make(chan Frame, 1)
	c.pendingMu.Lock()
	c.pending[f.ID] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, f.ID)
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

// CallDecode 发送 call，成功时把 body 解码到 dst。
func (c *Client) CallDecode(ctx context.Context, f Frame, dst any) error {
	resp, err := c.Call(ctx, f)
	if err != nil {
		return err
	}
	return resp.DecodeBody(dst)
}

// Stream 发送 call，返回流式帧通道。end=true 的帧到达后关闭。
func (c *Client) Stream(ctx context.Context, f Frame) (<-chan Frame, error) {
	f.Normalize()
	if err := f.Validate(); err != nil {
		return nil, err
	}
	ch := make(chan Frame, 16)
	c.pendingMu.Lock()
	c.streams[f.ID] = ch
	c.pendingMu.Unlock()
	if err := c.send(f); err != nil {
		c.pendingMu.Lock()
		delete(c.streams, f.ID)
		c.pendingMu.Unlock()
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
			stream := c.streams[f.ReplyTo]
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- f
				continue
			}
			if stream != nil {
				stream <- f
				c.pendingMu.Lock()
				delete(c.streams, f.ReplyTo)
				c.pendingMu.Unlock()
				close(stream)
				continue
			}
			if c.onEvent != nil {
				c.onEvent(f)
			}
		case TypeStream:
			c.pendingMu.Lock()
			stream := c.streams[f.ReplyTo]
			c.pendingMu.Unlock()
			if stream != nil {
				stream <- f
				if f.Stream != nil && f.Stream.End {
					c.pendingMu.Lock()
					delete(c.streams, f.ReplyTo)
					c.pendingMu.Unlock()
					close(stream)
				}
				continue
			}
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

var _ = http.StatusOK
