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
	"strings"
	"sync"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/service"
)

// Config 是 reasoner 的启动配置（引导只靠它：自己的名字/地址 + 设置中心地址）。
type Config struct {
	Name              string        // 服务名（注册用）
	Listen            string        // 监听地址 host:port
	Center            string        // 设置中心 WS 地址
	HeartbeatInterval time.Duration // 心跳间隔（默认 15s）
	Memory            string        // 记忆服务名（默认 memory——“完全启动”链的另一半）
	Images            string        // 图片服务名（默认 images——推理前取图内联）
	LLM               llm.Client    // 初始 LLM 客户端（测试注入 Fake 用；启动 Key 非空时由服务按 LLMBase/LLMKey/LLMModel 重建）
	LLMBase           string        // LLM API 地址（llm.base 的启动值）
	LLMModel          string        // LLM 模型名（llm.model 的启动值）
	LLMKey            string        // LLM API Key 的启动值（-llm-key / 环境变量；空 = 未配置）
	ToolConcurrency   int           // 单会话工具并发上限（默认 10；负值 = 不限制；同一服务可并发重复）
	ToolTimeout       time.Duration // 单个工具调用的执行超时（默认 60s）
	NotifyMode        string        // 工具结果通知模式："each"（逐条唤醒，默认）/ "batch"（攒批唤醒）；设置中心 tool.notify_mode 覆盖此项
	Reflect           bool          // 交付后是否复盘写经验（零值关闭；main.go 打开。测试默认不打开：复盘会多花一次模型调用）
	SecretFile        string        // 秘密项（llm.key）的本地持久化路径（空 = data/<name>-secret.json；"-" = 不落盘）
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

	asyncMu      sync.Mutex // 异步登记、待回执及早到补报共用锁（见 async.go）
	asyncJobs    map[asyncKey]*asyncJob
	asyncPending map[*asyncJob]bool
	asyncEarly   map[asyncKey][]protocol.AsyncResultPayload
	asyncDone    map[asyncKey]time.Time
	holdMu       sync.Mutex // user hold state; see hold.go
	holds        map[string]*holdState

	entryMu sync.Mutex        // 会话入口表锁（交付失败时往哪补一条）
	entries map[string]string // 会话 → 入口服务名（user_message 的 from）

	memSeq int64 // 会话记录的单调序号（atomic）：并发写（Fire 是 goroutine）也按真实先后落库

	setMu   sync.Mutex    // 运行时可设项锁（设置下发 / 拉取即时生效）
	notify  string        // 工具结果通知模式（each / batch）
	conc    int           // 单会话工具并发上限（-1 = 不限制）
	timeout time.Duration // 单个工具调用的执行超时
	reflect bool          // 交付后是否复盘写经验（reflect.on）

	liveMu   sync.RWMutex // 可热改运行配置快照锁（LLM 客户端 / 记忆与图片服务名）
	llm      llm.Client   // 当前 LLM 客户端（llm.base/model/key 任一变化即重建；Key 为空 = nil）
	llmBase  string       // 当前 LLM API 地址（重建用）
	llmModel string       // 当前 LLM 模型名（重建用）
	llmKey   string       // 当前 LLM API Key（重建用；空 = 无 Key）
	memory   string       // 当前记忆服务名
	images   string       // 当前图片服务名
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
		sems:         map[string]chan struct{}{},
		board:        map[string]*taskState{},
		asyncJobs:    map[asyncKey]*asyncJob{},
		asyncPending: map[*asyncJob]bool{},
		asyncEarly:   map[asyncKey][]protocol.AsyncResultPayload{},
		asyncDone:    map[asyncKey]time.Time{},
		holds:        map[string]*holdState{},
		entries:      map[string]string{},
		notify:       cfg.NotifyMode,
		conc:         cfg.ToolConcurrency,
		timeout:      cfg.ToolTimeout,
		reflect:      cfg.Reflect,

		llm:      cfg.LLM,
		memory:   cfg.Memory,
		images:   cfg.Images,
		llmBase:  cfg.LLMBase,
		llmModel: cfg.LLMModel,
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "对话",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		SecretFile:        cfg.SecretFile,
		Description:       "LLM 单步推理：任务先经记忆服务托管，收到来自记忆的 resume 才完全启动（调 LLM API）",
		Provides: []protocol.Provide{
			{Action: "user_message", Input: "用户消息文本 {text}", Output: "受理回执（最终结果由 deliver 另行送达）"},
			{Action: "think", Input: "单步推理 {prompt, system?, model?, max_tokens?}", Output: "{text}（一次补全：不带工具、不开任务、不进黑板；所有非任务型模型调用的统一出口）"},
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
	}, service.Label("工具调用的提示方式"),
		service.Hint("工具跑完要不要马上告诉你"),
		service.As("enum", "each", "batch"))
	s.HandleSetting("tool.concurrency", strconv.Itoa(cfg.ToolConcurrency), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || (n != -1 && n < 1) {
			return fmt.Errorf("需要 -1 或 ≥1 的整数，收到 %q", v)
		}
		s.setToolConcurrency(n)
		return nil
	}, service.Label("同时跑几个工具"),
		service.Hint("填 -1 表示不限制；填太大会同时压很多请求"),
		service.As("number"),
		service.Unit("count"))
	s.HandleSetting("tool.timeout_ms", strconv.FormatInt(cfg.ToolTimeout.Milliseconds(), 10), func(v string) error {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1000 {
			return fmt.Errorf("需要 ≥1000 的整数（毫秒），收到 %q", v)
		}
		s.setToolTimeout(time.Duration(n) * time.Millisecond)
		return nil
	}, service.Label("单次工具调用超时"),
		service.Hint("到点还没回来就算这次失败"),
		service.As("number"),
		service.Unit("ms"))
	s.HandleSetting("reflect.on", strconv.FormatBool(cfg.Reflect), func(v string) error {
		b, err := strconv.ParseBool(strings.TrimSpace(v))
		if err != nil {
			return fmt.Errorf("需要 true / false，收到 %q", v)
		}
		s.setReflectOn(b)
		return nil
	}, service.Label("交付后复盘写经验"),
		service.Hint("关掉之后每次交付不再多花一次模型调用；已经写进经验网的不会删"),
		service.As("bool"))
	// 推理侧可热改项：LLM 三件套 + 记忆 / 图片服务名（预设 = main.go 的启动 flag）。
	s.HandleSetting("llm.base", cfg.LLMBase, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空地址")
		}
		s.setLLMBase(v)
		return nil
	}, service.Label("模型接口地址"),
		service.Hint("OpenAI 兼容的 base URL，例如 https://api.deepseek.com"),
		service.As("text"))
	s.HandleSetting("llm.model", cfg.LLMModel, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空模型名")
		}
		s.setLLMModel(v)
		return nil
	}, service.Label("用哪个模型"),
		service.Hint("模型名要和上面的接口匹配，例如 deepseek-flash"),
		service.As("text"))
	s.HandleSetting("memory", cfg.Memory, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空服务名")
		}
		s.setMemoryName(v)
		return nil
	}, service.Label("记忆服务名"),
		service.Hint("对话历史与长期记忆交给哪个服务"),
		service.As("service"))
	s.HandleSetting("images", cfg.Images, func(v string) error {
		if strings.TrimSpace(v) == "" {
			return errors.New("不接受空服务名")
		}
		s.setImagesName(v)
		return nil
	}, service.Label("图片服务名"),
		service.Hint("需要看图时交给哪个服务"),
		service.As("service"))
	// 秘密项：值不进中心（面板提交那一次由中心转发过来），由本服务自己落盘 / 加载（Config.SecretFile）。
	s.HandleSecretWithCommit("llm.key", nil, s.setLLMKey,
		service.Label("模型 API Key"),
		service.Hint("只在你自己这边保存，中心不落盘也不回显；换 key 直接覆盖"))
	if cfg.LLMKey != "" {
		// 启动参数先作为运行时初值使用，不因面板首次保存失败而被替换。
		// 秘密文件里若有值，Start 加载时会覆盖（文件才是面板提交的最新状态）。
		s.setLLMKey(cfg.LLMKey)
	}
	// 只读项：启动参数带起来的，进面板可见但不可改（改它需要重启）。
	s.HandleReadOnly("name", cfg.Name)
	s.HandleReadOnly("addr", cfg.Listen)
	s.HandleReadOnly("center", cfg.Center)
	s.Handle("echo", s.actionEcho)
	s.Handle("debug_jump", s.actionDebugJump)
	s.Handle("think", s.actionThink)
	s.Handle("user_message", s.actionUserMessage)
	s.Handle("step", s.actionStep)
	s.Handle("resume", s.actionResume)
	s.Handle("report_result", s.actionReportResult)
	// 用户停手（入口服务的控制面，不是模型工具）：见 hold.go。
	s.Handle("user_hold", s.actionUserHold)
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

// reflectOn 返回"交付后要不要复盘写经验"。
func (s *Server) reflectOn() bool {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.reflect
}

// setReflectOn 开关复盘（设置下发 / 拉取调用）。
func (s *Server) setReflectOn(v bool) {
	s.setMu.Lock()
	s.reflect = v
	s.setMu.Unlock()
}

// ---------- 可热改运行配置（LLM / 记忆 / 图片） ----------
// cfg 里的 Memory / Images / LLM 只作启动初值；运行期一律走这里的快照读写。

// rebuildLLMLocked 用快照里的 base/key/model 重建 LLM 客户端（Key 为空 = 置 nil，轮次停住）。
// 调用方需持有 liveMu 写锁。
func (s *Server) rebuildLLMLocked() {
	if s.llmKey == "" {
		s.llm = nil
		return
	}
	s.llm = llm.NewDeepSeekClient(s.llmBase, s.llmKey, s.llmModel)
}

// llmClient 返回当前 LLM 客户端（nil = Key 未配置，轮次停住）。
func (s *Server) llmClient() llm.Client {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.llm
}

// setLLMBase 更新 LLM API 地址并立即重建客户端。
func (s *Server) setLLMBase(v string) {
	s.liveMu.Lock()
	s.llmBase = v
	s.rebuildLLMLocked()
	s.liveMu.Unlock()
}

// setLLMModel 更新 LLM 模型名并立即重建客户端。
func (s *Server) setLLMModel(v string) {
	s.liveMu.Lock()
	s.llmModel = v
	s.rebuildLLMLocked()
	s.liveMu.Unlock()
}

// setLLMKey 更新 LLM API Key 并立即重建客户端（空值 = 清除，客户端置 nil）。
// 面板变更由保存后的 commit 回调进入；构造启动初值时也使用此函数。
func (s *Server) setLLMKey(v string) {
	s.liveMu.Lock()
	s.llmKey = v
	s.rebuildLLMLocked()
	s.liveMu.Unlock()
}

// memoryName 返回当前记忆服务名（recall / store / resume 来源校验用）。
func (s *Server) memoryName() string {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.memory
}

// setMemoryName 更新记忆服务名（后续调用走新名）。
func (s *Server) setMemoryName(v string) {
	s.liveMu.Lock()
	s.memory = v
	s.liveMu.Unlock()
}

// imagesName 返回当前图片服务名（取图前解析用）。
func (s *Server) imagesName() string {
	s.liveMu.RLock()
	defer s.liveMu.RUnlock()
	return s.images
}

// setImagesName 更新图片服务名（后续取图走新名）。
func (s *Server) setImagesName(v string) {
	s.liveMu.Lock()
	s.images = v
	s.liveMu.Unlock()
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
