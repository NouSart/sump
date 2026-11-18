// Package experience 是共享经验数据的权威服务：长期记忆、经验网与会话目录。
//
// 边界：
//   - 这里没有会话库——对话原文 / 任务 / work / 整理池都在 session 服务；
//     整理池通过 SPv2 的 session.staged_pending|mark|retry 读写，会话标题不跨域读取。
//   - 记忆注入的拼装归 memory 门面，本服务只回答"经验四节"（核心 / 相关 / 线头 / 其他会话）。
//   - 本包不 import memory/server、memory/session、memory/store：跨域只走 SPv2。
package experience

import (
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/NouSart/sump/memory/shared"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

// Config 是 experience 服务的启动配置。
type Config struct {
	Name              string            // 服务名（默认 experience）
	Listen            string            // 监听地址 host:port
	Center            string            // 设置中心 WS 地址（Standalone=false 时生效）
	Standalone        bool              // 独立模式：只启动 /spv2，不连设置中心、不注册名册
	Peers             map[string]string // 静态内部对端（服务名 → ws://.../spv2）
	Shared            *shared.Store     // 共享经验库（长期记忆 / 经验网 / 会话目录）
	Embedding         string            // embedding 服务名（默认 embedding；不可用时静默回落 bigram）
	Reasoner          string            // reasoner 服务名（默认 reasoner；记忆整理调用 think）
	SharedPath        string            // 共享库路径（只读项展示用）
	HeartbeatInterval time.Duration     // 心跳间隔（默认 15s）
	Logger            *log.Logger       // 日志（New 的 logger 为空时用它；再空则 log.Default）
}

// 本域默认值。
const (
	defaultName          = "experience"
	defaultCoreLimit     = 3   // 核心记忆条数上限
	defaultCoreMaxChars  = 400 // 核心记忆字符预算
	defaultRelLimit      = 5   // 相关记忆条数上限
	defaultRelMaxChars   = 800 // 相关记忆字符预算
	defaultShareSessions = 5   // 会话目录注入条数
	defaultShareChars    = 120 // 会话摘要单条字数
)

// recallMinScore 是语义召回的最小余弦分（实测分数带——正例 ≥0.49、干扰 p95 ≤0.41；见 embedding/README）。
const recallMinScore = 0.45

// embedTimeout 是单次 embed 调用的超时：超时视为不可用，静默回落 bigram（不阻塞召回）。
const embedTimeout = 3 * time.Second

// netSeedCooldown 是"补种子向量"失败后的冷却时间。上游挂住时每次尝试都要等满
// embedTimeout，而且所有 net 动作都堵在同一把锁上——冷却一下，别让每条经验都去撞墙。
const netSeedCooldown = 30 * time.Second

// nowFunc 统一取时间：便于测试替换（经验网的遗忘曲线全看它）。
var nowFunc = time.Now

// Server 是 experience 服务实例（服务骨架 + 共享经验库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu         sync.Mutex // 运行时可设项（设置下发即时生效）
	consolidateMu sync.Mutex // 同一进程内串行整理：避免两个批次重复读同一批 pending
	coreLimit     int        // 核心记忆条数上限
	coreMaxChars  int        // 核心记忆字符预算
	relLimit      int        // 相关记忆条数上限
	relMaxChars   int        // 相关记忆字符预算
	embedding     string     // 当前 embedding 服务名（context.embedding 热改）
	shareSessions int        // 会话目录注入条数（share.sessions_limit）
	shareChars    int        // 会话摘要单条字数（share.digest_chars）

	// 经验网的种子向量（懒补齐）：只在五个大类还没有定义向量时做一次，
	// 失败就下次再试——embedding 服务此刻可能还没上线。
	// 但"下次"不能是"每一条经验都试一次"：上游挂住时每次都要等满一个 embedTimeout，
	// 而且这段时间里所有 net 动作都堵在这把锁上。所以失败后冷却一小会儿再试。
	netSeedMu   sync.Mutex
	netSeeded   bool
	netSeedNext time.Time // 冷却到期时间（零值 = 可以立刻试）

	staged stagedStore // 整理池访问器（只走 session 服务的内部 SPv2 method）
}

// New 创建实例并注册动作（对外 method 进 provides，内部 method 只注册）。
// 本服务不启动 session 的 work sweep；Standalone=true 时骨架只启动 /spv2。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.Name == "" {
		cfg.Name = defaultName
	}
	if cfg.Embedding == "" {
		cfg.Embedding = "embedding"
	}
	if cfg.Reasoner == "" {
		cfg.Reasoner = "reasoner"
	}
	if logger == nil {
		logger = cfg.Logger
	}
	if logger == nil {
		logger = log.Default()
	}
	s := &Server{
		cfg: cfg, logger: logger,
		coreLimit:     defaultCoreLimit,
		coreMaxChars:  defaultCoreMaxChars,
		relLimit:      defaultRelLimit,
		relMaxChars:   defaultRelMaxChars,
		embedding:     cfg.Embedding,
		shareSessions: defaultShareSessions,
		shareChars:    defaultShareChars,
	}
	s.staged = remoteStaged{s: s}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:             "经验",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		Standalone:        cfg.Standalone,
		Peers:             cfg.Peers,
		SettingFilter:     experienceSetting,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "共享经验服务：长期记忆（remember/forget；语义召回 + bigram 兜底）与经验网（note/dig/lineheads/prune）、会话目录（publish_session/shared_sessions）；不持有会话库",
		Provides:          provides(),
		Logger:            logger,
	})
	// 可设置项：只声明本域——注入预算、语义召回与共享目录；库路径是库身份（运行中换库不做）。
	s.HandleSetting("context.core_limit", strconv.Itoa(defaultCoreLimit), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		s.setMu.Lock()
		s.coreLimit = n
		s.setMu.Unlock()
		return nil
	}, service.Label("最多注入几条核心记忆"),
		service.Hint("你在长期记忆里标成「核心」的条目，每次对话最多带几条"),
		service.As("number"),
		service.Unit("count"))
	s.HandleSetting("context.core_max_chars", strconv.Itoa(defaultCoreMaxChars), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		s.setMu.Lock()
		s.coreMaxChars = n
		s.setMu.Unlock()
		return nil
	}, service.Label("核心记忆的总字数上限"),
		service.Hint("核心记忆太多会挤占对话空间，这里卡一个总字数"),
		service.As("number"),
		service.Unit("chars"))
	s.HandleSetting("context.rel_limit", strconv.Itoa(defaultRelLimit), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		s.setMu.Lock()
		s.relLimit = n
		s.setMu.Unlock()
		return nil
	}, service.Label("最多召回几条相关记忆"),
		service.Hint("按语义相关度挑出来的长期记忆，最多带几条（不相关的不带）"),
		service.As("number"),
		service.Unit("count"))
	s.HandleSetting("context.rel_max_chars", strconv.Itoa(defaultRelMaxChars), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		s.setMu.Lock()
		s.relMaxChars = n
		s.setMu.Unlock()
		return nil
	}, service.Label("相关记忆的总字数上限"),
		service.Hint("相关记忆太多会挤占对话空间，这里卡一个总字数"),
		service.As("number"),
		service.Unit("chars"))
	// embedding 服务名：热改（空 = 关闭语义召回，静默回落 bigram）。
	s.HandleSetting("context.embedding", cfg.Embedding, func(v string) error {
		s.setEmbeddingName(v)
		return nil
	}, service.Label("用哪个服务算语义向量"),
		service.Hint("关掉就退回关键词匹配——不花算力，但只认字面相近的记忆"),
		service.As("optional-service"))
	// 共享经验库的注入预算：目录是背景信息，宁可少给，别挤占对话。
	s.HandleSetting("share.sessions_limit", strconv.Itoa(defaultShareSessions), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		if n > 20 {
			return fmt.Errorf("最多 20 个会话，收到 %d", n)
		}
		s.setShareSessions(n)
		return nil
	}, service.Label("带上几个别的会话"),
		service.Hint("每轮回答前，把别的会话「在做什么」的摘要带几条；0 = 不带"),
		service.As("number"),
		service.Unit("count"))
	s.HandleSetting("share.digest_chars", strconv.Itoa(defaultShareChars), func(v string) error {
		n, err := parseNonNeg(v)
		if err != nil {
			return err
		}
		if n < 20 || n > 500 {
			return fmt.Errorf("需要 20~500 的整数（字符），收到 %d", n)
		}
		s.setShareChars(n)
		return nil
	}, service.Label("单条会话摘要的字数上限"),
		service.Hint("太长会挤占对话空间；这些摘要本来就该是一句话"),
		service.As("number"),
		service.Unit("chars"))
	// 只读项：启动参数带起来的（换它要重启）。共享库是库身份——运行中换库不做。
	s.HandleReadOnly("shared_db", cfg.SharedPath)
	// 全部动作的 SPv2.1 原生入口（/spv2 直接解 body）。
	s.registerNative()
	return s
}

// provides 是本域对外的动作名册（schema 描述沿用旧 memory 服务的原话）。
// 内部 method（inject / settings_schema）不进这里。
func provides() []protocol.Provide {
	return []protocol.Provide{
		{Action: "note", Input: "经验 {content, conversation_id?, task_id?, durability?}（core | durable | transient）", Output: "{id, node_ids, duplicate, skipped}（不暴露给模型：整理由复盘流程调；duplicate = 同一件事又说了一遍）"},
		{Action: "dig", Tool: true, Input: "{entry_id?, node_id?, limit?}（entry_id = 翻同簇兄弟，node_id = 展开整个大类）", Output: "{notes[], total}（顺着线头往下扒）"},
		{Action: "lineheads", Input: "{query, limit?}", Output: "{heads:[{node_id, text, count, score, task_id, members}]}（一行一条；recall 里自动带）"},
		{Action: "prune", Input: "{}", Output: "{entries, links}（软剪枝：只标状态、只断干枯的线，不删行）"},
		{Action: "remember", Tool: true, Input: "长期记忆条目 {kind, content, priority?, salience?}（kind 如 偏好/身份/知识/事件；priority>0 = 核心记忆，每次对话都会带上；salience 0..1 = 重要性，越大遗忘越慢）", Output: "{id, created}（同 kind+content 已有 active 条目时幂等跳过，salience/priority 只升不降）"},
		{Action: "consolidate", Input: "{}（整理池里有待处理候选时调用）", Output: "{processed, archived, skipped, failed, memnet_failed, conflicts, superseded, unresolved}（不暴露给模型：分类与冲突判断由 reasoner 完成）"},
		{Action: "publish_session", Input: "任务摘要 {conversation_id, task_id, title?, goal?, outcome?, capabilities?, private?}（复盘调用；已脱敏，不暴露给模型）", Output: "{published, private}"},
		{Action: "shared_sessions", Tool: true, Input: "{limit?, exclude?, query?}（别的会话在做什么的摘要；看不到它们的对话）", Output: "{sessions:[{entry, title, tasks:[{title, goal, outcome, capabilities, created_at}]}]}"},
		{Action: "shared_forget", Input: "{session_id?, task_id?}（删共享物；不暴露给模型）", Output: "{sessions, tasks}"},
		{Action: "forget", Input: "{id}（长期记忆条目 id）", Output: "{id}（删掉一条不再对的记忆）"},
	}
}

// experienceSetting 是本域负责的设置键：注入预算、语义召回、共享目录与共享库路径。
func experienceSetting(key string) bool {
	switch key {
	case "context.core_limit", "context.core_max_chars",
		"context.rel_limit", "context.rel_max_chars", "context.embedding",
		"share.sessions_limit", "share.digest_chars", "shared_db":
		return true
	default:
		return false
	}
}

// parseNonNeg 解析非负整数（设置项校验）。
func parseNonNeg(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("需要 ≥0 的整数，收到 %q", v)
	}
	return n, nil
}

// limits 返回当前生效的注入预算（锁内快照）。
func (s *Server) limits() (core, coreChars, rel, relChars int) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.coreLimit, s.coreMaxChars, s.relLimit, s.relMaxChars
}

// embeddingName 返回当前 embedding 服务名（空 = 语义召回关闭，回落 bigram）。
func (s *Server) embeddingName() string {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.embedding
}

// setEmbeddingName 更新 embedding 服务名（设置下发 / 拉取调用）。
func (s *Server) setEmbeddingName(v string) {
	s.setMu.Lock()
	s.embedding = strings.TrimSpace(v)
	s.setMu.Unlock()
}

// shareLimits 返回当前的目录预算。
func (s *Server) shareLimits() (sessions, chars int) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.shareSessions, s.shareChars
}

func (s *Server) setShareSessions(v int) {
	s.setMu.Lock()
	s.shareSessions = v
	s.setMu.Unlock()
}

func (s *Server) setShareChars(v int) {
	s.setMu.Lock()
	s.shareChars = v
	s.setMu.Unlock()
}
