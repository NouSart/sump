package protocol

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// ErrConnClosed 表示连接已关闭。
var ErrConnClosed = errors.New("protocol: connection closed")

// dialTimeout 是握手上限：死服务快速失败，避免默认 45s 的长时间挂起。
const dialTimeout = 5 * time.Second

// Client 是一条 WS 客户端连接：请求-响应配对 + 事件回调。
//
// M1 最小实现：不做断线重连与连接时长自适应（后续批次实现）。
// 连接持有一条读循环 goroutine：response 按 id 派发给调用方，
// 其余消息（事件）交给 onEvent 回调。
type Client struct {
	conn    *websocket.Conn
	writeMu sync.Mutex // gorilla 不支持并发写，写侧统一加锁

	pendingMu sync.Mutex
	pending   map[string]chan Envelope

	onEvent func(Envelope)

	closed    chan struct{}
	closeOnce sync.Once
}

// Dial 建立到 addr 的连接并启动读循环；onEvent 可为 nil。
func Dial(addr string, onEvent func(Envelope)) (*Client, error) {
	return DialContext(context.Background(), addr, onEvent)
}

// DialContext 同 Dial，但拨号受 ctx 约束（取消 / 超时立即返回，可中断等待）。
func DialContext(ctx context.Context, addr string, onEvent func(Envelope)) (*Client, error) {
	d := *websocket.DefaultDialer
	d.HandshakeTimeout = dialTimeout
	conn, _, err := d.DialContext(ctx, addr, nil)
	if err != nil {
		return nil, err
	}
	c := &Client{
		conn:    conn,
		pending: make(map[string]chan Envelope),
		onEvent: onEvent,
		closed:  make(chan struct{}),
	}
	go c.readLoop()
	return c, nil
}

// Send 发送一条消息，不等待回复（事件、响应或长耗时请求）。
func (c *Client) Send(env Envelope) error {
	data, err := Marshal(env)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.conn.WriteMessage(websocket.BinaryMessage, data); err != nil {
		c.close()
		return err
	}
	return nil
}

// Call 发送请求并等待同 id 的 response，或 ctx 超时 / 连接关闭。
func (c *Client) Call(ctx context.Context, env Envelope) (Envelope, error) {
	ch := make(chan Envelope, 1)
	c.pendingMu.Lock()
	c.pending[env.ID] = ch
	c.pendingMu.Unlock()
	defer func() {
		c.pendingMu.Lock()
		delete(c.pending, env.ID)
		c.pendingMu.Unlock()
	}()

	if err := c.Send(env); err != nil {
		return Envelope{}, err
	}
	select {
	case resp := <-ch:
		return resp, nil
	case <-ctx.Done():
		return Envelope{}, ctx.Err()
	case <-c.closed:
		return Envelope{}, ErrConnClosed
	}
}

// Close 主动关闭连接。
func (c *Client) Close() error {
	c.close()
	return nil
}

// Done 返回连接关闭信号（读循环结束或主动关闭时关闭）。
// 供上层监视连接健康（如断线自愈）。
func (c *Client) Done() <-chan struct{} {
	return c.closed
}

func (c *Client) readLoop() {
	defer c.close()
	for {
		_, data, err := c.conn.ReadMessage()
		if err != nil {
			return
		}
		env, err := Unmarshal(data)
		if err != nil {
			// 坏帧 = 协议损坏：无法归属到任何在途请求，直接关闭连接让它们立即失败
			//（调用方不再干等到超时；上层发现断开后按断线自愈重连）。
			c.close()
			return
		}
		if env.Type == TypeResponse {
			c.pendingMu.Lock()
			ch := c.pending[env.ID]
			c.pendingMu.Unlock()
			if ch != nil {
				ch <- env
			}
			continue
		}
		if c.onEvent != nil {
			c.onEvent(env)
		}
	}
}

func (c *Client) close() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}
