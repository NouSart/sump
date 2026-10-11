// Package server 是设置中心的 WS 服务端：接受连接、分发消息。
package server

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/settings-center/hub"
)

// Config 是设置中心的启动配置。
type Config struct {
	Addr         string        // 监听地址（host:port；":0" 可随机端口）
	SettingsPath string        // 设置覆盖值文件路径（空 = 不落盘）
	Heartbeat    time.Duration // 心跳间隔预期（默认 15s；服务未声明心跳时用于离线判定）
	Sweep        time.Duration // 离线扫描间隔（默认 5s）

	// OnReport 是错误上报的消费挂点（可选）：每收到一条 report 调用一次。
	// nil = 只记日志；未来前端 / 落盘 / 富日志由此接入。
	OnReport func(from string, r protocol.ReportPayload)
}

// Server 是设置中心服务实例。
type Server struct {
	cfg      Config
	hub      *hub.Hub
	settings *hub.Settings
	httpSrv  *http.Server
	ln       net.Listener
	logger   *log.Logger

	done chan struct{} // Shutdown 时关闭（停止离线扫描）
}

var upgrader = websocket.Upgrader{
	// 服务间互访，本机 / 内网使用，放开 Origin 校验。
	CheckOrigin: func(*http.Request) bool { return true },
}

// Start 启动设置中心（非阻塞）。
func Start(cfg Config, logger *log.Logger) (*Server, error) {
	if cfg.Heartbeat <= 0 {
		cfg.Heartbeat = 15 * time.Second
	}
	if cfg.Sweep <= 0 {
		cfg.Sweep = 5 * time.Second
	}
	ln, err := net.Listen("tcp", cfg.Addr)
	if err != nil {
		return nil, err
	}
	h := hub.New(logger)
	st, err := hub.NewSettings(cfg.SettingsPath, logger)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	h.PutSelf(protocol.ServiceCard{
		Name:        "settings-center",
		Addr:        "ws://" + ln.Addr().String() + "/ws",
		Description: "服务注册 · 全局信息 · 设置与调度",
		Provides: []protocol.Provide{
			{Action: "ping", Output: "pong（连通性测试）"},
			{Action: "roster", Output: "最新全量服务清单（DNS 式拉取）"},
			{Action: "report", Input: "{kind?, message}", Output: "回执（中心记日志并交给消费挂点）"},
		},
	})
	s := &Server{cfg: cfg, hub: h, settings: st, ln: ln, logger: logger, done: make(chan struct{})}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.handleWS)
	s.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Printf("服务退出: %v", err)
		}
	}()
	go s.sweepLoop()
	logger.Printf("settings-center 监听 ws://%s/ws", ln.Addr())
	return s, nil
}

// sweepLoop 周期扫描心跳超时：判离线则摘除名册并广播。
func (s *Server) sweepLoop() {
	t := time.NewTicker(s.cfg.Sweep)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			if roster, changed := s.hub.Sweep(time.Now(), s.cfg.Heartbeat); changed {
				s.hub.Broadcast(roster)
			}
		}
	}
}

// Addr 返回实际监听地址（host:port）。
func (s *Server) Addr() string { return s.ln.Addr().String() }

// Shutdown 关闭服务。
func (s *Server) Shutdown() error {
	select {
	case <-s.done:
	default:
		close(s.done)
	}
	return s.httpSrv.Close()
}

func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Printf("升级失败: %v", err)
		return
	}
	sc := &serverConn{ws: ws, hub: s.hub, settings: s.settings, srv: s, logger: s.logger}
	go sc.serve()
}

// serverConn 是一条已升级的服务连接。
type serverConn struct {
	ws       *websocket.Conn
	hub      *hub.Hub
	settings *hub.Settings
	srv      *Server // 回指（configure 推送等）
	logger   *log.Logger

	mu     sync.Mutex // 写锁（gorilla 不支持并发写）
	name   string     // 注册后的服务名（未注册为空）
	closed bool
}

// Send 发送一条消息（线程安全）。
func (c *serverConn) Send(env protocol.Envelope) error {
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
func (c *serverConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	return c.ws.Close()
}

func (c *serverConn) serve() {
	defer func() {
		if c.name != "" {
			c.hub.Disconnect(c.name, c)
			c.logger.Printf("连接断开: %s", c.name)
		}
		_ = c.Close()
	}()
	for {
		_, data, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			c.logger.Printf("坏帧: %v", err)
			continue
		}
		c.dispatch(env)
	}
}

func (c *serverConn) dispatch(env protocol.Envelope) {
	switch env.Type {
	case protocol.TypeRegister:
		c.handleRegister(env)
	case protocol.TypeHeartbeat:
		c.handleHeartbeat(env)
	case protocol.TypeJump:
		c.handleJump(env)
	default:
		c.replyError(env, "unsupported message type: "+env.Type)
	}
}

func (c *serverConn) handleRegister(env protocol.Envelope) {
	var reg protocol.RegisterPayload
	if err := env.DecodePayload(&reg); err != nil {
		c.replyError(env, "bad register payload")
		return
	}
	if reg.Name == "" || reg.Addr == "" {
		c.replyError(env, "register: name and addr required")
		return
	}

	c.name = reg.Name
	roster, changed := c.hub.Register(reg.Card(), c)
	c.settings.Declare(reg.Name, reg.Settings)

	data, err := protocol.EncodePayload(roster)
	if err != nil {
		c.replyError(env, "encode roster failed")
		return
	}
	resp, err := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: data})
	if err == nil {
		_ = c.Send(resp)
	}
	if changed {
		c.hub.Broadcast(roster)
		c.logger.Printf("注册: %s (%s)", reg.Name, reg.Addr)
	} else {
		c.logger.Printf("重连: %s（名片无变化，不广播）", reg.Name)
	}
}

// handleHeartbeat 处理心跳：bye = 优雅下线（立即摘除）；其余 = 报活（必要时从墓碑复归）。
func (c *serverConn) handleHeartbeat(env protocol.Envelope) {
	var hb protocol.HeartbeatPayload
	_ = env.DecodePayload(&hb)
	if hb.Status == "bye" {
		if roster, changed := c.hub.MarkOffline(env.From); changed {
			c.hub.Broadcast(roster)
			c.logger.Printf("下线: %s（告别）", env.From)
		}
		return
	}
	if roster, resurrected := c.hub.NoteSeen(env.From, c); resurrected {
		c.hub.Broadcast(roster)
	}
}

func (c *serverConn) handleJump(env protocol.Envelope) {
	var jp protocol.JumpPayload
	if err := env.DecodePayload(&jp); err != nil {
		c.replyError(env, "bad jump payload")
		return
	}
	switch jp.Action {
	case "ping":
		c.replyData(env, map[string]any{"pong": true, "who": "settings-center"})
	case "roster":
		// DNS 式拉取：返回最新全量清单（推送丢失 / 服务重启后自愈用）
		c.replyData(env, c.hub.Snapshot())
	case "list_settings":
		c.handleListSettings(env, jp)
	case "set_setting":
		c.handleSetSetting(env, jp)
	case "reset_setting":
		c.handleResetSetting(env, jp)
	case "report":
		c.handleReport(env, jp)
	default:
		c.replyError(env, "unknown action: "+jp.Action)
	}
}

// handleReport 处理错误上报（服务 → 中心）：记一条日志，并交给消费挂点（OnReport）。
// 防刷由上报方退避负责；中心视为永远在线，不排队、不回补、不落盘（挂点未来接前端 / 落盘）。
func (c *serverConn) handleReport(env protocol.Envelope, jp protocol.JumpPayload) {
	var p protocol.ReportPayload
	if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
		c.replyError(env, "bad report payload")
		return
	}
	if p.Message == "" {
		c.replyError(env, "report: message 必填")
		return
	}
	kind := p.Kind
	if kind == "" {
		kind = "-"
	}
	c.logger.Printf("上报 %s [%s]: %s", env.From, kind, p.Message)
	if fn := c.srv.cfg.OnReport; fn != nil {
		fn(env.From, p)
	}
	c.replyData(env, map[string]any{"status": "ok"})
}

// handleListSettings 返回设置清单（可选按服务过滤）。
func (c *serverConn) handleListSettings(env protocol.Envelope, jp protocol.JumpPayload) {
	var p protocol.SettingsListPayload
	if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
		c.replyError(env, "bad list_settings payload")
		return
	}
	c.replyData(env, protocol.SettingsResult{Services: c.settings.List(p.Service)})
}

// handleSetSetting 写覆盖值（服务与设置项都必须已被声明）；成功后向所属服务推送 configure。
func (c *serverConn) handleSetSetting(env protocol.Envelope, jp protocol.JumpPayload) {
	var p protocol.SetSettingPayload
	if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
		c.replyError(env, "bad set_setting payload")
		return
	}
	ss, err := c.settings.Set(p.Service, p.Key, p.Value)
	if err != nil {
		c.replyError(env, err.Error())
		return
	}
	c.logger.Printf("设置变更: %s.%s = %q", p.Service, p.Key, p.Value)
	c.srv.pushConfigure(p.Service, ss, p.Key)
	c.replyData(env, protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

// handleResetSetting 清除覆盖值，回落默认值。
func (c *serverConn) handleResetSetting(env protocol.Envelope, jp protocol.JumpPayload) {
	var p protocol.ResetSettingPayload
	if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
		c.replyError(env, "bad reset_setting payload")
		return
	}
	ss, err := c.settings.Reset(p.Service, p.Key)
	if err != nil {
		c.replyError(env, err.Error())
		return
	}
	c.logger.Printf("设置重置: %s.%s → 默认值", p.Service, p.Key)
	c.srv.pushConfigure(p.Service, ss, p.Key)
	c.replyData(env, protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

// pushConfigure 把某个设置项的变更生效值以 configure 跳转推送给所属服务。
// 尽力送达：失败仅记日志（服务侧仍保留“启动拉取”作兜底）。异步进行，不阻塞响应。
func (s *Server) pushConfigure(service string, ss protocol.ServiceSettings, key string) {
	var item protocol.SettingView
	found := false
	for _, v := range ss.Settings {
		if v.Key == key {
			item, found = v, true
			break
		}
	}
	if !found {
		return
	}
	card, ok := s.hub.Lookup(service)
	if !ok {
		s.logger.Printf("configure 未推送：服务 %s 不在名册（可能已离线）", service)
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		c, err := protocol.DialContext(ctx, card.Addr, nil)
		if err != nil {
			s.logger.Printf("configure 推送 %s 失败（拨号）: %v", service, err)
			return
		}
		defer c.Close()
		raw, err := protocol.EncodePayload(protocol.ConfigurePayload{
			Key: item.Key, Value: item.Value, Default: item.Default, Overridden: item.Overridden,
		})
		if err != nil {
			return
		}
		env, err := protocol.NewEnvelope(protocol.TypeJump, "settings-center", service, "", protocol.JumpPayload{Action: "configure", Input: raw})
		if err != nil {
			return
		}
		if _, err := c.Call(ctx, env); err != nil {
			s.logger.Printf("configure 推送 %s 失败: %v", service, err)
			return
		}
		s.logger.Printf("configure 已推送: %s.%s = %q", service, item.Key, item.Value)
	}()
}

// replyData 构造并发送一个成功响应（data 为任意可编码值）。
func (c *serverConn) replyData(req protocol.Envelope, v any) {
	data, err := protocol.EncodePayload(v)
	if err != nil {
		c.replyError(req, "encode failed")
		return
	}
	resp, err := protocol.NewResponse(req, "settings-center", protocol.ResponsePayload{OK: true, Data: data})
	if err == nil {
		_ = c.Send(resp)
	}
}

func (c *serverConn) replyError(req protocol.Envelope, msg string) {
	resp, err := protocol.NewResponse(req, "settings-center", protocol.ResponsePayload{OK: false, Error: msg})
	if err != nil {
		return
	}
	_ = c.Send(resp)
}
