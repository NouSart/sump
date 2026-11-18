// Package facade 是 memory 域唯一对外的服务：卡片、路由、设置/健康聚合。
// 它不持有任何数据库，也不 import session/experience 的实现；跨域只走 SPv2。
package facade

import (
	"context"
	"errors"
	"fmt"
	"log"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
	"github.com/vmihailenco/msgpack/v5"
)

// Config 是门面的启动配置。数据库路径不在这里：门面不打开数据库，
// 只读项的值从内部服务的 settings_schema 取。
type Config struct {
	Name              string            // 服务名（默认 memory）
	Listen            string            // 监听地址 host:port
	Center            string            // 设置中心 WS 地址
	Standalone        bool              // 测试/本地：只启动 /spv2，不注册设置中心
	Peers             map[string]string // 内部服务名 → ws://.../spv2
	HeartbeatInterval time.Duration     // 心跳间隔
	Logger            *log.Logger
}

// Server 是 memory 门面。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger
}

// New 创建门面。method / 设置项在 Start 的自检里按内部卡片注册。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Name == "" {
		cfg.Name = "memory"
	}
	if logger == nil {
		logger = cfg.Logger
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, logger: logger}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "记忆",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		Standalone:        cfg.Standalone,
		Peers:             cfg.Peers,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "记忆服务：对外唯一卡片；context 拼装 + session/experience 路由",
		Provides:          provides(),
		MetricsExtra:      s.peerMetrics,
		Logger:            logger,
	})
	return s
}

// Start 先做内部自检与聚合，再启动服务端。
func (s *Server) Start(ctx context.Context) error {
	if err := s.bootstrap(ctx); err != nil {
		return err
	}
	return s.Service.Start(ctx)
}

// bootstrap 拉取内部卡片与设置声明，完成 method/设置聚合后再对外。
func (s *Server) bootstrap(ctx context.Context) error {
	cards := map[string]spv2.Card{}
	schemas := map[string][]protocol.Setting{}
	for _, name := range []string{"session", "experience"} {
		card, err := s.peerCard(ctx, name)
		if err != nil {
			return fmt.Errorf("门面自检: %s 不可用: %w", name, err)
		}
		if card.Name != name {
			return fmt.Errorf("门面自检: 对端 %s 自报名 %s 不一致", name, card.Name)
		}
		cards[name] = card
		settings, err := s.peerSettings(ctx, name)
		if err != nil {
			return fmt.Errorf("门面自检: 读取 %s 设置声明失败: %w", name, err)
		}
		schemas[name] = settings
		s.AddHealthCheck(s.peerHealth(name))
	}
	if err := s.registerMethods(cards); err != nil {
		return err
	}
	if err := s.registerSettings(schemas); err != nil {
		return err
	}
	return nil
}

// registerMethods 把内部服务的对外 method 原样挂到门面；内部 method 必须被挡住。
func (s *Server) registerMethods(cards map[string]spv2.Card) error {
	owner := map[string]string{}
	for _, name := range []string{"session", "experience"} {
		for _, m := range cards[name].Methods {
			if isControlMethod(m.Name) || internalMethods[m.Name] {
				continue
			}
			if prev, ok := owner[m.Name]; ok {
				return fmt.Errorf("门面自检: method %s 同时由 %s 与 %s 声明", m.Name, prev, name)
			}
			owner[m.Name] = name
			s.HandleNative(m, s.forward(name, m.Name))
		}
	}
	for method := range proxyMethods {
		if owner[method] == "" {
			return fmt.Errorf("门面自检: 没有内部服务声明 method %s", method)
		}
	}
	for method := range owner {
		if !proxyMethods[method] {
			return fmt.Errorf("门面自检: 内部服务声明了未知 method %s", method)
		}
	}
	// context 没有数据权威，只在门面内拼装。
	s.HandleNative(contextMethod(), s.nativeContext)
	return nil
}

// registerSettings 按固定顺序合并 13 个键；同一键两个内部服务声明即失败。
func (s *Server) registerSettings(schemas map[string][]protocol.Setting) error {
	owner := map[string]string{}
	values := map[string]protocol.Setting{}
	for _, name := range []string{"session", "experience"} {
		for _, setting := range schemas[name] {
			if prev, ok := owner[setting.Key]; ok {
				return fmt.Errorf("门面自检: 设置键 %s 同时由 %s 与 %s 声明", setting.Key, prev, name)
			}
			owner[setting.Key] = name
			values[setting.Key] = setting
		}
	}
	for _, key := range settingOrder {
		if _, ok := internalSettingOwner[key]; ok {
			if _, ok := values[key]; !ok {
				return fmt.Errorf("门面自检: 内部服务未声明设置键 %s", key)
			}
			continue
		}
	}
	// 内部服务不得声明门面自有键。
	for key := range owner {
		if _, ok := internalSettingOwner[key]; !ok {
			return fmt.Errorf("门面自检: 内部服务声明了未知设置键 %s", key)
		}
	}
	for _, key := range settingOrder {
		if key == "name" || key == "addr" || key == "center" {
			continue
		}
		if key == "db" || key == "shared_db" {
			setting := values[key]
			s.HandleReadOnly(key, setting.Default, settingOptions(setting)...)
			continue
		}
		setting := values[key]
		s.HandleSetting(key, setting.Default, s.forwardSetting(key), settingOptions(setting)...)
	}
	s.HandleReadOnly("name", s.cfg.Name)
	s.HandleReadOnly("addr", s.cfg.Listen)
	s.HandleReadOnly("center", s.cfg.Center)
	return nil
}

// forwardSetting 按固定归属把设置下发给内部服务。
func (s *Server) forwardSetting(key string) func(string) error {
	peer := internalSettingOwner[key]
	return func(value string) error {
		if peer == "" {
			return fmt.Errorf("设置 %s 没有内部归属", key)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_, err := s.Call(ctx, peer, "configure", protocol.ConfigurePayload{Key: key, Value: value}, "")
		if err != nil {
			return fmt.Errorf("转发设置 %s 到 %s 失败: %w", key, peer, err)
		}
		return nil
	}
}

// forward 把入参 body 与帧头 from/work 原样交给数据归属服务。
func (s *Server) forward(peer, method string) service.NativeHandler {
	return func(ctx context.Context, call *spv2.Call) error {
		raw, err := s.CallWithRef(ctx, call.Frame.From, peer, method, msgpack.RawMessage(call.Frame.Body), call.Frame.Work)
		if err != nil {
			return replyForwardError(call, peer, method, err)
		}
		return call.Reply(msgpack.RawMessage(raw))
	}
}

func replyForwardError(call *spv2.Call, peer, method string, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return call.ReplyError(e.Code, e.Message, e.Retryable, e.Details)
	}
	return call.ReplyError(spv2.CodeUnavailable, fmt.Sprintf("%s.%s 不可用: %v", peer, method, err), true, nil)
}

func settingOptions(setting protocol.Setting) []service.Option {
	opts := []service.Option{service.Label(setting.Label), service.Hint(setting.Hint)}
	if setting.Kind != "" {
		opts = append(opts, service.As(setting.Kind, setting.Options...))
	}
	if setting.Unit != "" {
		opts = append(opts, service.Unit(setting.Unit))
	}
	return opts
}

func isControlMethod(name string) bool {
	switch name {
	case "hello", "health", "metrics", "configure":
		return true
	default:
		return false
	}
}
