// Package session 是会话私有数据的权威服务：对话记录、任务、work 账本与
// A2A 队列、上下文原材料、整理池。
//
// 边界：只碰会话私有库（memory/store）。共享经验（长期记忆 / 经验网 / 会话目录）
// 不在这里——那是 experience 域的事，本包不 import 它的任何实现；两个域之间
// 只有 SPv2 协议。上下文组装只发生在 context_base：纯函数 assemble 只吃会话库快照，
// 不注入共享记忆（记忆四节仍由 experience 提供，由 facade 在快照之上现注入）。
package session

import (
	"fmt"
	"log"
	"strconv"
	"sync"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

// Config 是 session 服务的启动配置。
type Config struct {
	Name              string            // 服务名（注册用，默认 session）
	Listen            string            // 监听地址 host:port
	Center            string            // 设置中心 WS 地址（Standalone=false 时生效）
	Standalone        bool              // 独立模式：只启动 /spv2，不连设置中心、不注册名册
	Peers             map[string]string // 静态内部对端（服务名 → ws://.../spv2）
	Store             *store.Store      // 会话库（私有：messages / tasks / conversations / work）
	HistoryLimit      int               // context_base 返回的最近记录条数（默认 12）
	DBPath            string            // 会话库路径（只读项展示用：换它要重启）
	HeartbeatInterval time.Duration     // 心跳间隔（默认 15s）
	Logger            *log.Logger       // 日志（New 的 logger 为空时用它；再空则 log.Default）
	// OwnerClient 是执行服务的可注入口（可选，默认 nil）。
	// nil 时 pause / resume 回 unavailable、call_status 不判 orphaned、cancel 走本地语义；
	// 生产实现由 main.go 的 session 角色在 Start 前用 SetOwnerClient 注入
	// （NewSPv2OwnerClient），不用 Config 传——客户端要拿本服务骨架做出站一跳。
	OwnerClient OwnerClient
}

// 本域默认值。
const (
	defaultName         = "session" // 服务名
	defaultHistoryLimit = 12        // context_base 带的最近消息条数
)

// Server 是 session 服务实例（服务骨架 + 会话库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu        sync.Mutex // 运行时可设项（设置下发即时生效）
	historyLimit int        // context_base 返回的最近消息条数

	ownerMu     sync.RWMutex // ownerClient 的注入/读取（Start 前后都可能问）
	ownerClient OwnerClient  // 执行服务客户端（可空：nil 有明确语义，见 owner_client.go）
}

// New 创建实例并注册动作（对外 method 进 provides，内部 method 只注册）。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Name == "" {
		cfg.Name = defaultName
	}
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = defaultHistoryLimit
	}
	if logger == nil {
		logger = cfg.Logger
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{cfg: cfg, logger: logger, historyLimit: cfg.HistoryLimit, ownerClient: cfg.OwnerClient}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "会话",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		Standalone:        cfg.Standalone,
		Peers:             cfg.Peers,
		SettingFilter:     sessionSetting,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "会话服务：对话记录、任务、work 账本与上下文原材料（会话私有数据；不组装记忆）",
		Provides:          provides(),
		Logger:            logger,
	})
	// 可设置项：只声明本域——历史条数可调；库路径是库身份（运行中换库不做）。
	s.HandleSetting("context.history_limit", strconv.Itoa(cfg.HistoryLimit), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		s.setMu.Lock()
		s.historyLimit = n
		s.setMu.Unlock()
		return nil
	}, service.Label("每次带上多少条旧对话"),
		service.Hint("每轮回答前，把最近这么多条对话一起交给模型；0 = 不带历史"),
		service.As("number"),
		service.Unit("count"))
	s.HandleReadOnly("db", cfg.DBPath)
	// 全部动作的 SPv2.1 原生入口（/spv2 直接解 body）。
	s.registerNative()
	return s
}

// provides 是本域对外的动作名册（schema 描述沿用 memory 服务的原话）。
// context_base 是内部 method（facade 调）；work / A2A / staged_* / settings_schema 同样不进这里。
func provides() []protocol.Provide {
	return []protocol.Provide{
		{Action: "store", Input: "{conversation_id, role, content}", Output: "落库回执"},
		{Action: "history", Input: "{conversation_id, limit}", Output: "{conversation_id, messages:[{role, content, at}]}（按时间正序；供界面回填，不做记忆组装）"},
		{Action: "conversation_list", Input: "{limit?, include_archived?}", Output: "{conversations:[{id, title, entry, status, created_at, updated_at}]}（按最近活跃排序；不暴露给模型 — 给界面列会话，不是给模型的工具）"},
		{Action: "conversation_rename", Input: "{id, title}（空标题 = 清掉名字）", Output: "{conversation}（不暴露给模型）"},
		{Action: "conversation_open", Input: "{id, title?, entry?}（幂等建会话；不暴露给模型）", Output: "{conversation}"},
		{Action: "conversation_archive", Input: "{id, archived}（归档 = 不进默认列表，不拦写入；不暴露给模型）", Output: "{conversation}"},
		// 任务面：reasoner 与门户按名字调用这些（`task_finish` / `task_resume` 就在交付与恢复路径上）。
		// 不声明它们的后果是"协议面看不到"：第三方按名册实现本服务时，永远不知道要实现它们。
		{Action: "task_current", Input: "{conversation_id}", Output: "{task?}（最近活跃的 active 任务；没有就是空）"},
		{Action: "task_open", Input: "{conversation_id, name}", Output: "{task}"},
		{Action: "task_finish", Input: "{task_id, name?}", Output: "{task}（status=done，之后只读）"},
		{Action: "task_rename", Input: "{task_id, name}", Output: "{task}"},
		{Action: "task_resume", Input: "{task_id}", Output: "{task, messages[]}（把未完结任务顶成当前，记录一并带走）"},
		{Action: "task_list", Input: "{conversation_id, limit?}", Output: "{tasks[]}（最近活跃在前）"},
		// work_open：建链前登记 work 与它所属的任务；context_base 只读这份归属（R-026 P2c）。
		{Action: "work_open", Input: "{work, conversation_id?, task_name?, boss?}（登记 work 并解析/创建所属任务；幂等）", Output: "{task_id}（不暴露给模型）"},
		// v2 上下文四态（内部 method，由 facade 的 context 调用）：按 work + 触发源取基座组装；
		// 同一触发复用同一快照（S-001 §3.1）。快照只存基座，experience 由 facade 现注入。
		// 不是工具：模型不该自己决定「要不要重开一轮」或「这轮算不算完」。
		{Action: "trigger_complete", Input: "{work, trigger:{kind,item}}（幂等；模型输出与后续动作安全落库后调用）", Output: "{completed}（不暴露给模型）"},
		// transcript（S-001 §3.2）：只读地把指定快照重放成文本；snapshot_id 必填，
		// 与 context_base 共用快照读取代码。不是工具：模型不自己决定历史要怎么重放。
		{Action: "transcript", Input: "{work, snapshot_id, budget:{max_chars?}}（只读；按快照渲染文本，snapshot_id 必填）", Output: "{text, truncated, omitted}（不暴露给模型）"},
		// v2 调用账本与常驻目录（R-023 P3）：都不是工具——模型不自己开调用、不自己判对账。
		{Action: "call_open", Input: "{work, service, action?, mode?, pausable?, ttl_ms?, exec_deadline_ms?}（派发前分配 call_id；本地意图 observe=waiting / exec=created）", Output: "{call_id}（不暴露给模型）"},
		{Action: "call_control", Input: "{id, control_id, op, ttl_ms?}（op=pause|resume|cancel；control_id 幂等；pause/resume 需 owner 确认）", Output: "{id, control_id, observe, exec, reason}（不暴露给模型）"},
		{Action: "call_status", Input: "{ids}(≤100)（对账读取；按 calls.remote_item_id 问 owner，未注入客户端时只读本地账本）", Output: "{owner_recovery_complete, items:[{id, observe, exec, phase, owner_service, updated_at}]}（不暴露给模型）"},
		{Action: "resident_list", Input: "{work?, limit?, cursor?}（列该 work 所属 owner 的常驻调用）", Output: "{items:[{id, kind, title, observe, exec, phase, owner_service, origin_session, created_at, updated_at}], next?}（不暴露给模型）"},
	}
}

// sessionSetting 是本域负责的设置键：历史条数可调；库路径只读。
func sessionSetting(key string) bool {
	return key == "context.history_limit" || key == "db"
}

// parseNonNeg 解析非负整数（设置项校验）。
func parseNonNeg(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("需要 ≥0 的整数，收到 %q", v)
	}
	return n, nil
}

// SetOwnerClient 注入执行服务客户端（生产实现见 owner_client_spv2.go）。
// 可在 Start 前后调用；控制与对账都通过 owner() 取当前值。
func (s *Server) SetOwnerClient(c OwnerClient) {
	s.ownerMu.Lock()
	s.ownerClient = c
	s.ownerMu.Unlock()
}

// owner 返回当前注入的 owner 客户端（nil = 未接 owner，调用点按 nil 的诚实语义处理）。
func (s *Server) owner() OwnerClient {
	s.ownerMu.RLock()
	defer s.ownerMu.RUnlock()
	return s.ownerClient
}

// currentHistoryLimit 返回当前生效的最近消息条数（锁内读）。
func (s *Server) currentHistoryLimit() int {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.historyLimit
}
