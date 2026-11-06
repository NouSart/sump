// Package server 是设置中心的 WS 服务端：接受连接、分发消息。
package server

import (
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/hub"
	"github.com/NouSart/sump/spv2"
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
	spv2Srv  *spv2.Server
	ln       net.Listener
	logger   *log.Logger

	sweepMS      atomic.Int64 // 离线扫描间隔（毫秒；可被设置覆盖）
	hbFallbackMS atomic.Int64 // 心跳判定兜底（毫秒；可被设置覆盖）

	done      chan struct{} // Shutdown 时关闭（停止离线扫描）
	sweepWake chan struct{} // 扫描间隔变更时唤醒扫描循环（立即按新间隔重排）
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
		Addr:        "ws://" + ln.Addr().String() + "/spv2",
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
	native, err := newNativeServer(s)
	if err != nil {
		_ = ln.Close()
		return nil, err
	}
	s.spv2Srv = native
	mux := http.NewServeMux()
	mux.Handle("/spv2", s.spv2Srv.Handler())
	s.httpSrv = &http.Server{Handler: mux}
	go func() {
		if err := s.httpSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
			logger.Printf("服务退出: %v", err)
		}
	}()
	go s.sweepLoop()
	logger.Printf("settings-center 监听 ws://%s/spv2", ln.Addr())
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

// pushAttempt 从名册取目标卡片，逐键下发；返回仍未送达的下标（服务明确拒绝记为结果，不重试）。
func (s *Server) pushAttempt(service string, items []protocol.SettingView, idxs []int, results []protocol.SettingResult) ([]int, error) {
	card, ok := s.hub.Lookup(service)
	if !ok {
		return idxs, fmt.Errorf("服务不在名册（可能已离线）")
	}
	return s.pushAttemptSPv2(service, card, items, idxs, results)
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
