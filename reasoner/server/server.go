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
	"strconv"
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

	asyncMu   sync.Mutex          // 异步受理登记表锁
	asyncJobs map[string]asyncJob // 受理句柄（item_id）→ 在途的异步工具调用（见 async.go）

	setMu   sync.Mutex    // 运行时可设项锁（设置下发 / 拉取即时生效）
	notify  string        // 工具结果通知模式（each / batch）
	conc    int           // 单会话工具并发上限（-1 = 不限制）
	timeout time.Duration // 单个工具调用的执行超时
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
	if cfg.ToolTimeout <= 0 {
		cfg.ToolTimeout = 60 * time.Second
	}
	if cfg.NotifyMode == "" {
		cfg.NotifyMode = "each"
	}
	s := &Server{
		cfg: cfg, logger: logger,
		sems:      map[string]chan struct{}{},
		board:     map[string]*taskState{},
		asyncJobs: map[string]asyncJob{},
		notify:    cfg.NotifyMode,
		conc:      cfg.ToolConcurrency,
		timeout:   cfg.ToolTimeout,
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
		Logger: logger,
	})
	// 可设置项：预设 = 本服务的启动引导值（来源由服务自己决定，见 main.go 的 flag）。
	s.HandleSetting("tool.notify_mode", cfg.NotifyMode, func(v string) error {
		if v != "each" && v != "batch" {
			return fmt.Errorf("不接受值 %q（可选 each / batch）", v)
		}
		s.setNotifyMode(v)
		return nil
	})
	s.HandleSetting("tool.concurrency", strconv.Itoa(cfg.ToolConcurrency), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || (n != -1 && n < 1) {
			return fmt.Errorf("需要 -1 或 ≥1 的整数，收到 %q", v)
		}
		s.setToolConcurrency(n)
		return nil
	})
	s.HandleSetting("tool.timeout_ms", strconv.FormatInt(cfg.ToolTimeout.Milliseconds(), 10), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			return fmt.Errorf("需要 ≥1000 的整数（毫秒），收到 %q", v)
		}
		s.setToolTimeout(time.Duration(n) * time.Millisecond)
		return nil
	})
	s.Handle("echo", s.actionEcho)
	s.Handle("debug_jump", s.actionDebugJump)
	s.Handle("user_message", s.actionUserMessage)
	s.Handle("step", s.actionStep)
	s.Handle("resume", s.actionResume)
	s.Handle("report_result", s.actionReportResult)
	return s
}

// Start 启动服务骨架；设置同步由骨架负责（注册后全量下发 + 拉取兜底）。
func (s *Server) Start(ctx context.Context) error {
	return s.Service.Start(ctx)
}

// toolTimeout 返回单个工具调用的执行超时（当前生效值）。
func (s *Server) toolTimeout() time.Duration {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if s.timeout <= 0 {
		return 60 * time.Second
	}
	return s.timeout
}

// setToolTimeout 更新单个工具调用的执行超时（设置下发 / 拉取调用）。
func (s *Server) setToolTimeout(d time.Duration) {
	s.setMu.Lock()
	s.timeout = d
	s.setMu.Unlock()
}

// toolConcurrency 返回单会话工具并发上限（-1 = 不限制）。
func (s *Server) toolConcurrency() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.conc
}

// setToolConcurrency 更新单会话工具并发上限（已建的会话信号量不缩容，新会话用新值）。
func (s *Server) setToolConcurrency(n int) {
	s.setMu.Lock()
	s.conc = n
	s.setMu.Unlock()
}

// notifyMode 返回工具结果通知模式："each"（逐条唤醒，默认）或 "batch"（攒批唤醒）。
func (s *Server) notifyMode() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	if s.notify == "batch" {
		return "batch"
	}
	return "each"
}

// setNotifyMode 设置通知模式（设置下发 / 拉取调用）。
func (s *Server) setNotifyMode(v string) {
	s.setMu.Lock()
	s.notify = v
	s.setMu.Unlock()
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
