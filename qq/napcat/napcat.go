// Package napcat 是 NapCat（OneBot 11）正向 WebSocket 客户端：
// 连接与断线重连（正常断开 1s；异常指数退避、上限 30s）、事件回调、动作调用（echo 配对响应）。
package napcat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Config 是客户端配置。
type Config struct {
	URL       string         // 正向 WS 地址（ws://host:port）
	Token     string         // access_token（可选，追加为查询参数）
	Logger    *log.Logger    // 日志（必填，New 时有默认）
	OnMessage func(ev Event) // 消息事件回调（独立 goroutine 中调用，勿阻塞）
}

// Event 是 OneBot 消息事件（仅取所需字段）。
type Event struct {
	PostType    string          `json:"post_type"`
	MessageType string          `json:"message_type"` // private / group
	UserID      int64           `json:"user_id"`
	GroupID     int64           `json:"group_id"`
	SelfID      int64           `json:"self_id"`
	RawMessage  string          `json:"raw_message"`
	Message     json.RawMessage `json:"message"` // 段数组 或 CQ 字符串
}

// Segment 是一条消息段。
type Segment struct {
	Type string         `json:"type"`
	Data map[string]any `json:"data"`
}

// ParseSegments 解析消息段（数组形式）；字符串形式返回 nil。
func ParseSegments(raw json.RawMessage) []Segment {
	var segs []Segment
	if err := json.Unmarshal(raw, &segs); err != nil {
		return nil
	}
	return segs
}

// Text 提取纯文本（段数组：text 段拼接；字符串形式：原样返回）。
func (ev Event) Text() string {
	var s string
	if json.Unmarshal(ev.Message, &s) == nil {
		return strings.TrimSpace(s)
	}
	var b strings.Builder
	for _, seg := range ParseSegments(ev.Message) {
		if seg.Type == "text" {
			if t, ok := seg.Data["text"].(string); ok {
				b.WriteString(t)
			}
		}
	}
	return strings.TrimSpace(b.String())
}

// ImageURLs 提取图片 URL。
func (ev Event) ImageURLs() []string {
	var urls []string
	for _, seg := range ParseSegments(ev.Message) {
		if seg.Type == "image" {
			if u, ok := seg.Data["url"].(string); ok && u != "" {
				urls = append(urls, u)
			}
		}
	}
	return urls
}

// FileRef 是一条文件附件的引用（下载 URL + 文件名）。
type FileRef struct {
	URL  string
	Name string
}

// FileRefs 提取文件附件（OneBot 11 file 段：data.url 下载地址；
// 文件名各实现不一——data.file / data.name 都认）。
func (ev Event) FileRefs() []FileRef {
	var out []FileRef
	for _, seg := range ParseSegments(ev.Message) {
		if seg.Type != "file" {
			continue
		}
		u, _ := seg.Data["url"].(string)
		if u == "" {
			continue
		}
		name, _ := seg.Data["file"].(string)
		if n, ok := seg.Data["name"].(string); ok && n != "" {
			name = n
		}
		out = append(out, FileRef{URL: u, Name: name})
	}
	return out
}

// Client 是 NapCat 客户端。
type Client struct {
	cfg Config

	writeMu sync.Mutex
	mu      sync.Mutex
	conn    *websocket.Conn
	pending map[string]chan map[string]any
	seq     uint64
}

// New 创建客户端。
func New(cfg Config) *Client {
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Client{cfg: cfg, pending: map[string]chan map[string]any{}}
}

// Run 是连接循环：直到 ctx 取消才返回；正常断开 1s 后重连，异常指数退避（上限 30s）。
func (c *Client) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		err := c.serve(ctx)
		if ctx.Err() != nil {
			return
		}
		wait := backoff
		if err == nil {
			wait = time.Second
			backoff = time.Second
		} else {
			c.cfg.Logger.Printf("napcat 断开：%v（%v 后重连）", err, wait)
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return
		}
	}
}

// endpoint 返回当前端点的原始地址与 token（加锁读：地址 / token 可被 SetEndpoint 热改）。
func (c *Client) endpoint() (rawURL, token string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cfg.URL, c.cfg.Token
}

// SetEndpoint 热更新 NapCat 地址与 access_token（设置下发用）：地址或 token 变化时断开当前连接，
// 让连接循环以新值重连（token 只在握手时用得上，光换字段不重连不会生效）。
func (c *Client) SetEndpoint(rawURL, token string) {
	c.mu.Lock()
	if c.cfg.URL == rawURL && c.cfg.Token == token {
		c.mu.Unlock()
		return
	}
	c.cfg.URL = rawURL
	c.cfg.Token = token
	conn := c.conn
	c.mu.Unlock()
	if conn != nil {
		_ = conn.Close() // 触发重连（Run 的退避从 1s 档起）
	}
}

func (c *Client) serve(ctx context.Context) error {
	rawURL, token := c.endpoint()
	addr := rawURL
	if token != "" {
		sep := "?"
		if strings.Contains(addr, "?") {
			sep = "&"
		}
		addr = addr + sep + "access_token=" + url.QueryEscape(token)
	}
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, addr, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.mu.Unlock()
	c.cfg.Logger.Printf("napcat 已连接：%s", rawURL)
	defer c.dropConn()

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			return err
		}
		c.dispatch(data)
	}
}

// dispatch 分发一条入站帧：先匹配动作响应（echo），再处理消息事件。
func (c *Client) dispatch(data []byte) {
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		return
	}
	if echo, ok := m["echo"].(string); ok && echo != "" {
		c.mu.Lock()
		ch := c.pending[echo]
		delete(c.pending, echo)
		c.mu.Unlock()
		if ch != nil {
			select {
			case ch <- m:
			default:
			}
		}
		return
	}
	if m["post_type"] == "message" && c.cfg.OnMessage != nil {
		var ev Event
		if err := json.Unmarshal(data, &ev); err == nil {
			// 独立 goroutine：消息处理（含 action 往返）不阻塞读循环，避免响应无法分发。
			go c.cfg.OnMessage(ev)
		}
	}
}

// Call 调用一个 OneBot 动作并等待响应（15s 超时；断开时返回错误）。
func (c *Client) Call(ctx context.Context, action string, params map[string]any) (map[string]any, error) {
	c.mu.Lock()
	conn := c.conn
	if conn == nil {
		c.mu.Unlock()
		return nil, errors.New("napcat 未连接")
	}
	c.seq++
	echo := strconv.FormatUint(c.seq, 10)
	ch := make(chan map[string]any, 1)
	c.pending[echo] = ch
	c.mu.Unlock()

	payload, err := json.Marshal(map[string]any{"action": action, "params": params, "echo": echo})
	if err != nil {
		c.removePending(echo)
		return nil, err
	}
	c.writeMu.Lock()
	werr := conn.WriteMessage(websocket.TextMessage, payload)
	c.writeMu.Unlock()
	if werr != nil {
		c.removePending(echo)
		return nil, werr
	}

	select {
	case resp := <-ch:
		if resp == nil {
			return nil, errors.New("napcat 连接断开")
		}
		if status, _ := resp["status"].(string); status != "" && status != "ok" {
			return resp, fmt.Errorf("napcat 返回 %s: %v", status, resp["message"])
		}
		return resp, nil
	case <-ctx.Done():
		c.removePending(echo)
		return nil, ctx.Err()
	case <-time.After(15 * time.Second):
		c.removePending(echo)
		return nil, errors.New("napcat 响应超时")
	}
}

func (c *Client) removePending(echo string) {
	c.mu.Lock()
	delete(c.pending, echo)
	c.mu.Unlock()
}

func (c *Client) dropConn() {
	c.mu.Lock()
	c.conn = nil
	pend := c.pending
	c.pending = map[string]chan map[string]any{}
	c.mu.Unlock()
	// 唤醒所有等待者（nil 表示连接断开）。
	for _, ch := range pend {
		select {
		case ch <- nil:
		default:
		}
	}
}

// Connected 报告当前是否已连接。
func (c *Client) Connected() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}
