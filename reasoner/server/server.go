package server

// reasoner 服务的核心：把单步推理相关动作注册到服务骨架（注册 / 心跳 /
// 名册 / 断线自愈 / 告别见 service 包）。任务上下文与在跑工具由内存黑板持有
// （board.go），窗口 = 任务在跑期；轮次由事件驱动（派发 / 结果注入 / 等待 /
// 收尾门禁），轮次间以自跳 step 衔接——服务本身不驻留循环。

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// Config 是 reasoner 的启动配置（引导只靠它：自己的名字/地址 + 设置中心地址）。
type Config struct {
	Name              string        // 服务名（注册用）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	Memory            string        // 记忆服务名（默认 memory——“完全启动”链的另一半）
	Images            string        // 图片服务名（默认 images——推理前取图内联）
	LLM               llm.Client    // 单步推理的 LLM 客户端（user_message 链必需；nil 时轮次停住）
	ToolConcurrency   int           // 单会话工具并发上限（默认 10；负值 = 不限制；同一服务可并发重复）
	ToolTimeout       time.Duration // 单个工具调用的执行超时（默认 60s）
	NotifyMode        string        // 工具结果通知模式："each"（逐条唤醒，默认）/ "batch"（攒批唤醒）；设置中心 tool.notify_mode 覆盖此项
}

// Server 是 reasoner 服务实例（服务骨架 + 推理配置）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	semsMu sync.Mutex               // 工具并发信号量表锁
	sems   map[string]chan struct{} // 会话 → 工具并发信号量（懒创建）

	boardMu sync.Mutex            // 黑板锁
	board   map[string]*taskState // 会话 → 进行中的任务（窗口 = 任务在跑期）

	notifyMu sync.Mutex // 通知模式锁
	notify   string     // 工具结果通知模式（each / batch）
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Memory == "" {
		cfg.Memory = "memory"
	}
	if cfg.Images == "" {
		cfg.Images = "images"
	}
	if cfg.ToolConcurrency == 0 {
		cfg.ToolConcurrency = 10
	}
	s := &Server{
		cfg: cfg, logger: logger,
		sems:   map[string]chan struct{}{},
		board:  map[string]*taskState{},
		notify: cfg.NotifyMode,
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "LLM 单步推理：任务先经记忆服务托管，收到来自记忆的 resume 才完全启动（调 LLM API）",
		Provides: []protocol.Provide{
			{Action: "user_message", Input: "用户消息文本 {text}", Output: "受理回执（最终结果由 deliver 另行送达）"},
			{Action: "echo", Input: "任意", Output: "原样返回（测试用）"},
			{Action: "debug_jump", Input: "{to, action, input}", Output: "目标服务的响应数据（调试用）"},
		},
		Settings: []protocol.Setting{
			{Key: "tool.notify_mode", Default: "each"},
		},
		Logger: logger,
	})
	s.Handle("echo", s.actionEcho)
	s.Handle("debug_jump", s.actionDebugJump)
	s.Handle("configure", s.actionConfigure)
	s.Handle("user_message", s.actionUserMessage)
	s.Handle("step", s.actionStep)
	s.Handle("resume", s.actionResume)
	return s
}

// Start 启动服务骨架，并同步拉取一次设置中心里本服务的设置覆盖值（覆盖不改变运行中的轮次）。
func (s *Server) Start(ctx context.Context) error {
	if err := s.Service.Start(ctx); err != nil {
		return err
	}
	s.pullSettings(ctx)
	return nil
}

// pullSettings 拉取设置中心里本服务的设置（目前只关心 tool.notify_mode 的覆盖值）。
// 拉取失败不阻塞启动：沿用本地默认（CLI 参数）。
func (s *Server) pullSettings(ctx context.Context) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	data, err := s.Call(cctx, "settings-center", "list_settings", protocol.SettingsListPayload{Service: s.cfg.Name}, s.cfg.Name, "")
	if err != nil {
		s.logger.Printf("拉取设置失败（沿用本地值 %s）: %v", s.notifyMode(), err)
		return
	}
	var res protocol.SettingsResult
	if err := protocol.DecodeRaw(data, &res); err != nil {
		s.logger.Printf("设置解析失败（沿用本地值 %s）: %v", s.notifyMode(), err)
		return
	}
	for _, svc := range res.Services {
		if svc.Service != s.cfg.Name {
			continue
		}
		for _, item := range svc.Settings {
			if item.Key == "tool.notify_mode" && item.Overridden && item.Value != "" {
				s.setNotifyMode(item.Value)
				s.logger.Printf("设置生效: tool.notify_mode=%s（来自设置中心）", item.Value)
			}
		}
	}
}

// actionConfigure 承接设置中心下发的设置变更（configure）：立即生效（不等重启）。
// 推送是尽力送达，启动时的 pullSettings 是兜底——两者幂等，重复应用无副作用。
func (s *Server) actionConfigure(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ConfigurePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	switch p.Key {
	case "tool.notify_mode":
		if p.Value != "each" && p.Value != "batch" {
			return nil, fmt.Errorf("configure: tool.notify_mode 不接受值 %q（可选 each / batch）", p.Value)
		}
		s.setNotifyMode(p.Value)
		s.logger.Printf("设置生效（下发）: tool.notify_mode=%s", p.Value)
		return map[string]any{"status": "applied"}, nil
	default:
		// 只回执“已应用”的事：未支持的设置键明确拒绝（中心侧会记推送失败）。
		return nil, fmt.Errorf("configure: 未支持的设置键 %q", p.Key)
	}
}

// toolTimeout 返回单个工具调用的执行超时（默认 60s）。
func (s *Server) toolTimeout() time.Duration {
	if s.cfg.ToolTimeout > 0 {
		return s.cfg.ToolTimeout
	}
	return 60 * time.Second
}

// notifyMode 返回工具结果通知模式："each"（逐条唤醒，默认）或 "batch"（攒批唤醒）。
func (s *Server) notifyMode() string {
	s.notifyMu.Lock()
	defer s.notifyMu.Unlock()
	if s.notify == "batch" {
		return "batch"
	}
	return "each"
}

// setNotifyMode 设置通知模式（启动时由设置中心覆盖值调用）。
func (s *Server) setNotifyMode(v string) {
	s.notifyMu.Lock()
	s.notify = v
	s.notifyMu.Unlock()
}

// ---------- 调试 / 测试动作 ----------

func (s *Server) actionEcho(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var v any
	if err := protocol.DecodeRaw(in.Input, &v); err != nil {
		return nil, err
	}
	return map[string]any{"who": s.cfg.Name, "echo": v}, nil
}

// actionDebugJump 替调用方向目标服务发起一次跳转（调试 / 测试用），
// 支持 to = "self"（自我跳转）或服务名（查名册解析地址）。
func (s *Server) actionDebugJump(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var req struct {
		To     string             `msgpack:"to"`
		Action string             `msgpack:"action"`
		Input  msgpack.RawMessage `msgpack:"input"`
	}
	if err := protocol.DecodeRaw(in.Input, &req); err != nil {
		return nil, err
	}
	if req.To == "" || req.Action == "" {
		return nil, errors.New("debug_jump: to 与 action 必填")
	}
	data, err := s.Call(ctx, req.To, req.Action, req.Input, env.Trace, "")
	if err != nil {
		return nil, err
	}
	if len(data) == 0 {
		return map[string]any{}, nil
	}
	return data, nil
}
