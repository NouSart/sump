// Package service 是 SUMP v2 的服务骨架（Go 便捷库，非服务）：
// 注册 / 心跳 / 名册 / 断线自愈（重连 + 重新注册）/ 优雅下线（bye）/ WS 服务端 / 跳转调用。
// 协议本身见 protocol 包；非 Go 服务只需遵循 SPEC.md，不必使用本库。
package service

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// Config 是服务启动配置（引导只靠它：自己的名字/地址 + 设置中心地址）。
type Config struct {
	Name              string             // 服务名（注册用）
	Listen            string             // 监听地址 host:port
	Center            string             // 设置中心 WS 地址
	HeartbeatInterval time.Duration      // 心跳间隔（默认 15s；随名片声明，中心据此判定离线）
	ActionTimeout     time.Duration      // 单次动作处理超时（默认 30s）
	Description       string             // 名片：功能简介
	Provides          []protocol.Provide // 名片：对外提供的动作
	Settings          []protocol.Setting // 名片：可设置项
	Logger            *log.Logger        // 日志（默认 log.Default）
}

// Handler 处理一次跳转动作，返回的数据会作为响应 payload 的 data。
type Handler func(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error)

// Service 是服务实例：一条到设置中心的连接（注册 / 心跳 / 名册）+ 一个 WS 服务端。
type Service struct {
	cfg    Config
	logger *log.Logger

	actions map[string]Handler

	mu      sync.Mutex
	roster  map[string]protocol.ServiceCard
	selfURL string
	center  *protocol.Client // 当前中心连接（nil = 未连接）
	closed  bool             // Shutdown 后为真（阻止继续重连）
	regs    int64            // 注册成功次数（日志用）

	repMu       sync.Mutex     // 错误上报退避锁
	repThrottle reportThrottle // 错误上报防刷（见 report.go）

	httpSrv *http.Server
}

// New 创建服务实例（尚未启动）。
func New(cfg Config) *Service {
	if cfg.HeartbeatInterval <= 0 {
		cfg.HeartbeatInterval = 15 * time.Second
	}
	if cfg.ActionTimeout <= 0 {
		cfg.ActionTimeout = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = log.Default()
	}
	return &Service{
		cfg:     cfg,
		logger:  cfg.Logger,
		actions: map[string]Handler{},
		roster:  map[string]protocol.ServiceCard{},
	}
}

// Handle 注册一个动作的处理函数。
func (s *Service) Handle(action string, fn Handler) {
	s.actions[action] = fn
}

// WsURL 返回自己的对外地址（Start 成功后有效）。
func (s *Service) WsURL() string { return s.selfURL }

// Roster 返回当前名册快照（按服务名排序）。
func (s *Service) Roster() []protocol.ServiceCard {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]protocol.ServiceCard, 0, len(s.roster))
	for _, c := range s.roster {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Start 启动 WS 服务端并**同步**接入设置中心（首次注册失败即返回错误——
// 引导配置错误要立刻可见）；之后由后台循环负责断线自愈（重连 + 重新注册）。
func (s *Service) Start(ctx context.Context) error {
	ln, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return err
	}
	s.selfURL = "ws://" + ln.Addr().String() + "/ws"

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	s.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			s.logger.Printf("%s 服务端退出: %v", s.cfg.Name, err)
		}
	}()
	s.logger.Printf("%s 监听 %s", s.cfg.Name, s.selfURL)

	if err := s.connectAndRegister(ctx); err != nil {
		_ = s.httpSrv.Close() // 注册失败即放弃启动：回收监听，不留“半个服务”
		return fmt.Errorf("注册失败: %w", err)
	}
	go s.watchCenter(ctx)
	return nil
}

// Shutdown 尽力告别（bye，中心立即摘除名册）并关闭服务端与中心连接。
func (s *Service) Shutdown() {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	c := s.center
	s.center = nil
	s.mu.Unlock()

	if c != nil {
		if env, err := protocol.NewEnvelope(protocol.TypeHeartbeat, s.cfg.Name, "settings-center", "",
			protocol.HeartbeatPayload{Status: "bye"}); err == nil {
			_ = c.Send(env) // 尽力：随后连接即关（TCP 有序，bye 先于关闭到达）
		}
		_ = c.Close()
	}
	if s.httpSrv != nil {
		_ = s.httpSrv.Close()
	}
}

// ---------- 到设置中心的连接：注册 / 心跳 / 断线自愈 ----------

// connectAndRegister 建立一条中心连接、注册并启动心跳。
func (s *Service) connectAndRegister(ctx context.Context) error {
	c, err := protocol.Dial(s.cfg.Center, s.onCenterEvent)
	if err != nil {
		return err
	}

	env, err := protocol.NewEnvelope(protocol.TypeRegister, s.cfg.Name, "settings-center", "", protocol.RegisterPayload{
		Name:        s.cfg.Name,
		Addr:        s.selfURL,
		Description: s.cfg.Description,
		Provides:    s.cfg.Provides,
		Settings:    s.cfg.Settings,
		HeartbeatMS: s.cfg.HeartbeatInterval.Milliseconds(),
	})
	if err != nil {
		_ = c.Close()
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	resp, err := c.Call(rctx, env)
	cancel()
	if err != nil {
		_ = c.Close()
		return err
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		_ = c.Close()
		return err
	}
	if !rp.OK {
		_ = c.Close()
		return errors.New("注册被拒: " + rp.Error)
	}
	var roster protocol.RosterPayload
	if err := protocol.DecodeRaw(rp.Data, &roster); err != nil {
		_ = c.Close()
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return errors.New("服务已关闭")
	}
	s.center = c
	s.regs++
	regs := s.regs
	s.mu.Unlock()

	s.setRoster(roster)
	if regs == 1 {
		s.logger.Printf("%s 已接入设置中心（名册 %d 个服务, rev %d）", s.cfg.Name, len(roster.Services), roster.Revision)
	} else {
		s.logger.Printf("%s 已重新接入设置中心（第 %d 次注册）", s.cfg.Name, regs)
	}
	go s.heartbeatLoop(ctx, c)
	return nil
}

// watchCenter 监视中心连接：断开后重连并重新注册（指数退避，上限 30s）。
func (s *Service) watchCenter(ctx context.Context) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		s.mu.Lock()
		closed := s.closed
		c := s.center
		s.mu.Unlock()
		if closed || ctx.Err() != nil {
			return
		}

		if c == nil {
			if err := s.connectAndRegister(ctx); err != nil {
				s.logger.Printf("%s 重连设置中心失败: %v（%v 后重试）", s.cfg.Name, err, backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			backoff = time.Second
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			// 连接断开：清空后快速重连（首试 1s，不做长退避）
			s.mu.Lock()
			if s.center == c {
				s.center = nil
			}
			s.mu.Unlock()
			s.logger.Printf("%s 与设置中心连接断开，1s 后重连", s.cfg.Name)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

// heartbeatLoop 向中心发心跳；写失败即关闭连接（触发自愈）。
func (s *Service) heartbeatLoop(ctx context.Context, c *protocol.Client) {
	t := time.NewTicker(s.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			return
		case <-t.C:
			env, err := protocol.NewEnvelope(protocol.TypeHeartbeat, s.cfg.Name, "settings-center", "", protocol.HeartbeatPayload{Status: "ok"})
			if err != nil {
				continue
			}
			if err := c.Send(env); err != nil {
				return // Send 内部已关闭连接 → watchCenter 重连
			}
		}
	}
}

func (s *Service) onCenterEvent(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeRoster:
		var r protocol.RosterPayload
		if err := env.DecodePayload(&r); err != nil {
			s.logger.Printf("roster 解析失败: %v", err)
			return
		}
		s.setRoster(r)
		s.logger.Printf("roster 更新: %d 个服务 (rev %d)", len(r.Services), r.Revision)
	default:
		s.logger.Printf("未知事件: %s", env.Type)
	}
}

func (s *Service) setRoster(r protocol.RosterPayload) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.roster = make(map[string]protocol.ServiceCard, len(r.Services))
	for _, c := range r.Services {
		s.roster[c.Name] = c
	}
}

// ---------- WS 服务端（接受 jump） ----------

var upgrader = websocket.Upgrader{
	CheckOrigin: func(*http.Request) bool { return true },
}

// outConn 是服务一侧的写封装（gorilla 不支持并发写）。
type outConn struct {
	ws     *websocket.Conn
	mu     sync.Mutex
	closed bool
}

// Send 发送一条消息（线程安全）。
func (c *outConn) Send(env protocol.Envelope) error {
	data, err := protocol.Marshal(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return protocol.ErrConnClosed
	}
	return c.ws.WriteMessage(websocket.BinaryMessage, data)
}

// Close 关闭连接。
func (c *outConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.ws.Close()
}

func (s *Service) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("升级失败: %v", err)
		return
	}
	go s.serveConn(ws)
}

// serveConn 读循环：jump 交给后台 goroutine 处理（慢动作不阻塞读循环）。
func (s *Service) serveConn(ws *websocket.Conn) {
	c := &outConn{ws: ws}
	defer c.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			s.logger.Printf("坏帧: %v", err)
			continue
		}
		if env.Type != protocol.TypeJump {
			s.replyError(c, env, "unsupported message type: "+env.Type)
			continue
		}
		go s.handleJump(c, env)
	}
}

func (s *Service) handleJump(c *outConn, env protocol.Envelope) {
	var jp protocol.JumpPayload
	if err := env.DecodePayload(&jp); err != nil {
		s.replyError(c, env, "bad jump payload")
		return
	}
	fn, ok := s.actions[jp.Action]
	if !ok {
		s.replyError(c, env, "unknown action: "+jp.Action)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ActionTimeout)
	defer cancel()
	out, err := fn(ctx, env, jp)
	if err != nil {
		s.replyError(c, env, err.Error())
		return
	}
	data, err := protocol.EncodePayload(out)
	if err != nil {
		s.replyError(c, env, "encode result failed")
		return
	}
	resp, err := protocol.NewResponse(env, s.cfg.Name, protocol.ResponsePayload{OK: true, Data: data})
	if err == nil {
		_ = c.Send(resp)
	}
}

func (s *Service) replyError(c *outConn, req protocol.Envelope, msg string) {
	resp, err := protocol.NewResponse(req, s.cfg.Name, protocol.ResponsePayload{OK: false, Error: msg})
	if err == nil {
		_ = c.Send(resp)
	}
}

// ---------- 出站：跳转调用 ----------

// Call 访问目标服务：发一跳并等响应（短连接）。to 支持 "self" 与服务名（查名册）。
// 只在“拨号失败”（连接未建立，消息可证明未送达）时安全重试：0.5s、2s；
// 送达后的失败（响应超时等）不重试——可能已被处理，重试需要幂等键（待定）。
func (s *Service) Call(ctx context.Context, to, action string, payload any, trace, boss string) (msgpack.RawMessage, error) {
	name, addr, err := s.Resolve(to)
	if err != nil {
		return nil, err
	}
	raw, err := encodeInput(payload)
	if err != nil {
		return nil, err
	}

	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			wait := 500 * time.Millisecond
			if attempt == 2 {
				wait = 2 * time.Second
			}
			select {
			case <-time.After(wait):
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		data, err := s.callOnce(ctx, name, addr, action, raw, trace, boss)
		if err == nil {
			return data, nil
		}
		lastErr = err
		if !errors.Is(err, errDialFailed) {
			break
		}
	}
	return nil, lastErr
}

// errDialFailed 标记“连接未建立”的失败（重试安全的唯一情形）。
var errDialFailed = errors.New("拨号失败")

// callOnce 执行一次跳转（不复用连接）。
func (s *Service) callOnce(ctx context.Context, name, addr, action string, raw msgpack.RawMessage, trace, boss string) (msgpack.RawMessage, error) {
	c, err := protocol.DialContext(ctx, addr, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: 连接 %s 失败: %v", errDialFailed, addr, err)
	}
	defer c.Close()
	env, err := protocol.NewEnvelope(protocol.TypeJump, s.cfg.Name, name, trace, protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		return nil, err
	}
	env.Boss = boss
	resp, err := c.Call(ctx, env)
	if err != nil {
		return nil, fmt.Errorf("跳转 %s(%s) 失败: %w", name, action, err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		return nil, err
	}
	if !rp.OK {
		return nil, errors.New("远端错误: " + rp.Error)
	}
	return rp.Data, nil
}

// Fire 后台发起一跳（发出即完；完成与否仅记日志）。
// 失败时向设置中心上报一条（kind=fire）——链路跳转未送达属于值得集中可见的错误。
func (s *Service) Fire(to, action string, payload any, trace, boss string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if _, err := s.Call(ctx, to, action, payload, trace, boss); err != nil {
			s.logger.Printf("跳转 %s(%s) 未确认送达: %v", to, action, err)
			if to != "settings-center" {
				s.Report("fire", fmt.Sprintf("跳转 %s(%s) 未送达: %v", to, action, err))
			}
		}
	}()
}

// Resolve 解析跳转目标：支持 "self" 与服务名（查名册）。
func (s *Service) Resolve(to string) (name, addr string, err error) {
	if to == "self" {
		return s.cfg.Name, s.selfURL, nil
	}
	s.mu.Lock()
	card, ok := s.roster[to]
	s.mu.Unlock()
	if !ok {
		return "", "", fmt.Errorf("未知目标服务: %s", to)
	}
	return card.Name, card.Addr, nil
}

// encodeInput 把任意载荷编码为 msgpack；已是 RawMessage 的原样透传。
func encodeInput(payload any) (msgpack.RawMessage, error) {
	if payload == nil {
		return nil, nil
	}
	if raw, ok := payload.(msgpack.RawMessage); ok {
		return raw, nil
	}
	return protocol.EncodePayload(payload)
}
