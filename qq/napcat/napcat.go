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
	// Sender 是发送者资料（OneBot 11 的 sender 对象）：昵称 / 群名片 / 群身份。
	Sender EventSender `json:"sender"`
}

// EventSender 是消息事件里的发送者资料。
// 群名片（card）比昵称更贴近"这个人在这个群里叫什么"，优先用它。
type EventSender struct {
	UserID   int64  `json:"user_id"`
	Nickname string `json:"nickname"`
	Card     string `json:"card"`
	Role     string `json:"role"` // 群里的身份：owner / admin / member（私聊为空）
}

// DisplayName 返回这个发送者该被怎么称呼：群名片优先，其次昵称，最后空串（不造假名字）。
func (ev Event) DisplayName() string {
	if card := strings.TrimSpace(ev.Sender.Card); card != "" {
		return card
	}
	return strings.TrimSpace(ev.Sender.Nickname)
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

// Mentions 报告这条消息有没有点到 selfID（群里的 @）。
//
// 段数组里看 at 段；字符串形式（CQ 码）退化为查 `[CQ:at,qq=<selfID>]`。
// selfID 未知（0）时一律返回 false——"不知道自己在群里叫什么"不能算被点到。
func (ev Event) Mentions(selfID int64) bool {
	if selfID == 0 {
		return false
	}
	want := strconv.FormatInt(selfID, 10)
	var s string
	if json.Unmarshal(ev.Message, &s) == nil {
		return strings.Contains(s, "[CQ:at,qq="+want+"]") || strings.Contains(s, "[CQ:at,qq="+want+",")
	}
	for _, seg := range ParseSegments(ev.Message) {
		if seg.Type != "at" {
			continue
		}
		// data.qq 在各实现里可能是字符串或数字；@全体成员 的 qq 是 "all"。
		switch v := seg.Data["qq"].(type) {
		case string:
			if v == want {
				return true
			}
		case float64:
			if int64(v) == selfID {
				return true
			}
		}
	}
	return false
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

// GroupInfo 是群资料（get_group_info 里用得上的部分）。
//
// 群名与成员数是**扩散面**的一部分：模型要知道"这句话多少人能看见"。
// 取不全就留零值——宁可少一个数字，不编一个数字。
type GroupInfo struct {
	GroupID int64
	Name    string
	Members int // 成员数（0 = 未知）
}

// GroupInfo 取群资料（NapCat 的 get_group_info）。
func (c *Client) GroupInfo(ctx context.Context, groupID int64) (GroupInfo, error) {
	resp, err := c.Call(ctx, "get_group_info", map[string]any{"group_id": groupID, "no_cache": false})
	if err != nil {
		return GroupInfo{GroupID: groupID}, err
	}
	data, _ := resp["data"].(map[string]any)
	info := GroupInfo{GroupID: groupID}
	if name, _ := data["group_name"].(string); name != "" {
		info.Name = name
	}
	// member_count 经 JSON 解出来是 float64；有的实现给的是字符串。
	switch v := data["member_count"].(type) {
	case float64:
		info.Members = int(v)
	case string:
		if n, err := strconv.Atoi(v); err == nil {
			info.Members = n
		}
	}
	if info.Name == "" && info.Members == 0 {
		return info, errors.New("get_group_info 没有返回可用的群名与成员数")
	}
	return info, nil
}
