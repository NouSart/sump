// Package server 是设置中心的 WS 服务端：接受连接、分发消息。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/hub"
)

// 设置中心自身的设置项（一切皆服务：中心也声明并应用自己的设置；本地应用，不经网络）。
const (
	selfServiceName            = "settings-center"
	settingSweepMS             = "sweep_ms"              // 离线扫描间隔（毫秒）
	settingHeartbeatFallbackMS = "heartbeat_fallback_ms" // 未声明心跳的服务判离线时的兜底间隔（毫秒）
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

	sweepMS      atomic.Int64 // 离线扫描间隔（毫秒；可被设置覆盖）
	hbFallbackMS atomic.Int64 // 心跳判定兜底（毫秒；可被设置覆盖）

	done      chan struct{} // Shutdown 时关闭（停止离线扫描）
	sweepWake chan struct{} // 扫描间隔变更时唤醒扫描循环（立即按新间隔重排）
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
		Name:        selfServiceName,
		Title:       "设置中心",
		Addr:        "ws://" + ln.Addr().String() + "/ws",
		Description: "服务注册 · 全局信息 · 设置与调度",
		Provides: []protocol.Provide{
			{Action: "ping", Output: "pong（连通性测试）"},
			{Action: "roster", Output: "最新全量服务清单（DNS 式拉取）"},
			{Action: "report", Input: "{kind?, message}", Output: "回执（中心记日志并交给消费挂点）"},
		},
	})
	// 中心自身也是服务：声明自己的可设置项（预设 = 本次启动的引导值）。
	// 后三项是只读项：启动参数带起来的（换它要重启），进面板只为看得见。
	st.Declare(selfServiceName, []protocol.Setting{
		{
			Key: settingSweepMS, Default: strconv.FormatInt(cfg.Sweep.Milliseconds(), 10),
			Label: "多久扫一遍谁掉线了", Hint: "按这个间隔检查一遍名册，超过时限没心跳的服务判为离线",
			Kind: "number", Unit: "ms",
		},
		{
			Key: settingHeartbeatFallbackMS, Default: strconv.FormatInt(cfg.Heartbeat.Milliseconds(), 10),
			Label: "等多久没动静算掉线", Hint: "服务自己没声明心跳间隔时，用这个时长兜底",
			Kind: "number", Unit: "ms",
		},
		{Key: "addr", Default: cfg.Addr, Label: "监听地址", ReadOnly: true},
		{Key: "settings", Default: cfg.SettingsPath, Label: "设置清单", ReadOnly: true},
	})
	s := &Server{cfg: cfg, hub: h, settings: st, ln: ln, logger: logger, done: make(chan struct{}), sweepWake: make(chan struct{}, 1)}
	s.sweepMS.Store(cfg.Sweep.Milliseconds())
	s.hbFallbackMS.Store(cfg.Heartbeat.Milliseconds())
	// 覆盖值启动即应用（本地生效；应用失败沿用引导值）。
	for _, it := range st.Items(selfServiceName) {
		if !it.Overridden {
			continue
		}
		if err := s.applySelfSetting(it.Key, it.Value); err != nil {
			logger.Printf("设置应用失败（启动，沿用引导值）: %s.%s: %v", selfServiceName, it.Key, err)
			continue
		}
		logger.Printf("设置生效（启动）: %s.%s = %q", selfServiceName, it.Key, it.Value)
	}
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
// 间隔取当前生效值；sweep_ms 变更时被唤醒，立即按新间隔重排（不等旧档位走完）。
func (s *Server) sweepLoop() {
	for {
		timer := time.NewTimer(time.Duration(s.sweepMS.Load()) * time.Millisecond)
		select {
		case <-s.done:
			timer.Stop()
			return
		case <-s.sweepWake:
			timer.Stop()
		case <-timer.C:
			fallback := time.Duration(s.hbFallbackMS.Load()) * time.Millisecond
			if roster, changed := s.hub.Sweep(time.Now(), fallback); changed {
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
	declChanged := c.settings.Declare(reg.Name, reg.Settings)

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
	// 注册即对齐（不阻塞注册响应）：声明的普通项 → 全量逐键下发（异步、有界重试）；
	// 只有秘密项 / 只读项（没有可对齐的普通项）→ 只广播一次事件（面板刷新）。
	if len(reg.Settings) > 0 {
		if items := c.settings.PushItems(reg.Name); len(items) > 0 {
			go c.srv.pushSettings(reg.Name, items, "register")
		} else if declChanged {
			c.srv.broadcastSettings(protocol.SettingsEventPayload{Cause: "register", Service: reg.Name})
		}
	} else if declChanged {
		c.srv.broadcastSettings(protocol.SettingsEventPayload{Cause: "register", Service: reg.Name})
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
	case "report_setting":
		c.handleReportSetting(env, jp)
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

// handleSetSetting 写覆盖值（服务与设置项都必须已被声明）；成功后按边界落地生效值。
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
	item, hasItem := viewOf(ss, p.Key)
	if hasItem && item.Secret {
		c.logger.Printf("设置变更: %s.%s（秘密，不回显）（来自 %s）", p.Service, p.Key, env.From)
	} else {
		c.logger.Printf("设置变更: %s.%s = %q（来自 %s）", p.Service, p.Key, p.Value, env.From)
	}
	c.srv.applyOrPush(p.Service, ss, p.Key, "set_setting", p.Value)
	c.replyData(env, protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

// handleResetSetting 清除覆盖值，回落服务声明的预设值。
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
	c.logger.Printf("设置重置: %s.%s → 预设值（来自 %s）", p.Service, p.Key, env.From)
	// 秘密项的重置 = 推一个空值过去，让服务自己清掉本地那份。
	c.srv.applyOrPush(p.Service, ss, p.Key, "reset_setting", "")
	c.replyData(env, protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

// handleReportSetting 承接「服务自报实际值」（服务侧的值自己变了）：只更新服务侧那一列，
// **不动**中心手里的覆盖值；秘密项只认「有没有值」。结果以 settings 事件广播（面板实时刷新）。
func (c *serverConn) handleReportSetting(env protocol.Envelope, jp protocol.JumpPayload) {
	var p protocol.ReportSettingPayload
	if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
		c.replyError(env, "bad report_setting payload")
		return
	}
	ss, err := c.settings.Report(p.Service, p.Key, p.Value, p.Set, p.Seq)
	if err != nil {
		c.replyError(env, err.Error())
		return
	}
	if item, found := viewOf(ss, p.Key); found && item.Secret {
		c.logger.Printf("设置回传: %s.%s（秘密，只回有没有值）（来自 %s）", p.Service, p.Key, env.From)
	} else {
		c.logger.Printf("设置回传: %s.%s = %q（来自 %s）", p.Service, p.Key, p.Value, env.From)
	}
	c.srv.broadcastSettings(protocol.SettingsEventPayload{
		Cause: "report_setting", Service: p.Service,
		Results: []protocol.SettingResult{{Key: p.Key, OK: true}},
	})
	c.replyData(env, protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

// applyOrPush 落地一次设置变更：设置中心自身 → 本地应用并广播结果；
// 其余服务 → 异步逐键下发（不阻塞调用方，结果与失败以 settings 事件回传）。
// submitted 是调用方这次提交的原文：秘密项的值只在这里透传一下，中心不保存它。
func (s *Server) applyOrPush(service string, ss protocol.ServiceSettings, key, cause, submitted string) {
	item, found := viewOf(ss, key)
	if !found {
		return
	}
	if service == selfServiceName {
		res := protocol.SettingResult{Key: key, Value: item.Value}
		if err := s.applySelfSetting(key, item.Value); err != nil {
			res.Error = err.Error()
			s.logger.Printf("设置应用失败（本地）: %s.%s: %v", service, key, err)
		} else {
			res.OK = true
			s.logger.Printf("设置生效（本地）: %s.%s = %q", service, key, item.Value)
		}
		s.broadcastSettings(protocol.SettingsEventPayload{Cause: cause, Service: service, Results: []protocol.SettingResult{res}})
		return
	}
	if item.Secret {
		item.Value = submitted
	}
	go s.pushSettings(service, []protocol.SettingView{item}, cause)
}

// viewOf 在设置视图里按 key 找一项。
func viewOf(ss protocol.ServiceSettings, key string) (protocol.SettingView, bool) {
	for _, v := range ss.Settings {
		if v.Key == key {
			return v, true
		}
	}
	return protocol.SettingView{}, false
}

// pushSettings 把某服务的设置项逐键以 configure 下发（有界重试：+0.5s、+2s），
// 完成后广播 settings 事件（结果回传 + 面板刷新）。服务明确拒绝不重试（拒绝也是结果）。
func (s *Server) pushSettings(service string, items []protocol.SettingView, cause string) {
	results := make([]protocol.SettingResult, len(items))
	for i, it := range items {
		results[i] = protocol.SettingResult{Key: it.Key}
	}
	remaining := make([]int, 0, len(items))
	for i := range items {
		remaining = append(remaining, i)
	}

	var lastErr error
	for attempt := 0; attempt < 3 && len(remaining) > 0; attempt++ {
		if attempt > 0 {
			wait := 500 * time.Millisecond
			if attempt == 2 {
				wait = 2 * time.Second
			}
			time.Sleep(wait)
		}
		remaining, lastErr = s.pushAttempt(service, items, remaining, results)
	}
	deliverErr := "未送达"
	if lastErr != nil {
		deliverErr = "未送达: " + lastErr.Error()
	}
	for _, i := range remaining {
		results[i].Error = deliverErr
	}

	// 秘密项：下发成功才算「服务侧有值」——中心据此显示 已设置/未设置（值本身永不上线）。
	secret := map[string]string{}
	for _, it := range items {
		if it.Secret {
			secret[it.Key] = it.Value
		}
	}
	for _, r := range results {
		if v, isSecret := secret[r.Key]; isSecret {
			if r.OK {
				s.settings.MarkSecretSet(service, r.Key, v != "")
				s.logger.Printf("configure 已推送: %s.%s（秘密，不回显）", service, r.Key)
			} else {
				s.logger.Printf("configure 推送未生效: %s.%s（秘密）: %s", service, r.Key, r.Error)
			}
			continue
		}
		if r.OK {
			s.settings.NotePushed(service, r.Key, r.Value)
			s.logger.Printf("configure 已推送: %s.%s = %q", service, r.Key, r.Value)
		} else {
			s.logger.Printf("configure 推送未生效: %s.%s: %s", service, r.Key, r.Error)
		}
	}
	s.broadcastSettings(protocol.SettingsEventPayload{Cause: cause, Service: service, Results: results})
}

// pushAttempt 用一条连接逐键下发；返回仍未送达的下标（服务明确拒绝记为结果，不重试）。
func (s *Server) pushAttempt(service string, items []protocol.SettingView, idxs []int, results []protocol.SettingResult) ([]int, error) {
	card, ok := s.hub.Lookup(service)
	if !ok {
		return idxs, errors.New("服务不在名册（可能已离线）")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := protocol.DialContext(ctx, card.Addr, nil)
	if err != nil {
		return idxs, fmt.Errorf("拨号失败: %v", err)
	}
	defer c.Close()

	var remaining []int
	var lastErr error
	for _, i := range idxs {
		it := items[i]
		raw, err := protocol.EncodePayload(protocol.ConfigurePayload{
			Key: it.Key, Value: it.Value, Default: it.Default, Overridden: it.Overridden,
		})
		if err != nil {
			results[i].Error = "编码失败: " + err.Error()
			continue
		}
		env, err := protocol.NewEnvelope(protocol.TypeJump, selfServiceName, service, "", protocol.JumpPayload{Action: "configure", Input: raw})
		if err != nil {
			results[i].Error = err.Error()
			continue
		}
		resp, err := c.Call(ctx, env)
		if err != nil {
			remaining = append(remaining, i)
			lastErr = err
			continue
		}
		var rp protocol.ResponsePayload
		if err := resp.DecodePayload(&rp); err != nil {
			results[i].Error = "响应解析失败: " + err.Error()
			continue
		}
		if !rp.OK {
			results[i].Error = rp.Error
			if results[i].Error == "" {
				results[i].Error = "响应缺少 ok"
			}
			continue
		}
		results[i].OK = true
		if !it.Secret {
			results[i].Value = it.Value
		}
	}
	return remaining, lastErr
}

// broadcastSettings 广播一条设置事件（面板据此刷新与提示；其余服务忽略）。
func (s *Server) broadcastSettings(p protocol.SettingsEventPayload) {
	s.hub.BroadcastEvent(protocol.TypeSettings, p)
}

// applySelfSetting 应用设置中心自身的设置项（本地生效，不经网络）。
func (s *Server) applySelfSetting(key, value string) error {
	switch key {
	case settingSweepMS:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 100 {
			return fmt.Errorf("需要 ≥100 的整数（毫秒），收到 %q", value)
		}
		s.sweepMS.Store(n)
		select { // 唤醒扫描循环：立即按新间隔重排
		case s.sweepWake <- struct{}{}:
		default:
		}
		return nil
	case settingHeartbeatFallbackMS:
		n, err := strconv.ParseInt(value, 10, 64)
		if err != nil || n < 100 {
			return fmt.Errorf("需要 ≥100 的整数（毫秒），收到 %q", value)
		}
		s.hbFallbackMS.Store(n)
		return nil
	default:
		return fmt.Errorf("未声明的设置项 %q", key)
	}
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
