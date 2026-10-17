// memory 服务的核心：把记忆动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包）。
// recall 收到调用方托管的任务上下文 → 查会话历史并组装记忆 → 以 resume 发回调用方（触发其“完全启动”）。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QAQ-awa-QAQ/sump/memory/compose"
	"github.com/QAQ-awa-QAQ/sump/memory/longterm"
	"github.com/QAQ-awa-QAQ/sump/memory/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// Config 是 memory 服务的启动配置。
type Config struct {
	Name              string          // 服务名（注册用，默认 memory）
	Listen            string          // 监听地址 host:port
	Center            string          // 设置中心 WS 地址
	HeartbeatInterval time.Duration   // 心跳间隔（默认 15s）
	Store             *store.Store    // 会话库
	Longterm          *longterm.Store // 长期记忆库（与会话库共享同一 SQLite 句柄）
	HistoryLimit      int             // recall 返回的最近消息条数（默认 12；作为声明预设上报）
	Embedding         string          // embedding 服务名（默认 embedding；语义召回不可用时静默回落 bigram）
	DBPath            string          // 数据库路径（只读项展示用：换它要重启）
}

// v0 注入预算的默认值（对齐 v1 量级；作为声明预设，可被设置中心覆盖）。
const (
	defaultHistoryLimit = 12  // recall 返回的最近消息条数
	defaultCoreLimit    = 3   // 核心记忆条数上限
	defaultCoreMaxChars = 400 // 核心记忆字符预算
	defaultRelLimit     = 5   // 相关记忆条数上限
	defaultRelMaxChars  = 800 // 相关记忆字符预算
)

// recallMinScore 是语义召回的最小余弦分（实测分数带——正例 ≥0.49、干扰 p95 ≤0.41；见 embedding/README）。
const recallMinScore = 0.45

// embedTimeout 是单次 embed 调用的超时：超时视为不可用，静默回落 bigram（不阻塞 recall）。
const embedTimeout = 3 * time.Second

// Server 是 memory 服务实例（服务骨架 + 会话库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger

	setMu        sync.Mutex // 运行时可设项（设置下发 / 拉取即时生效）
	historyLimit int        // recall 返回的最近消息条数
	coreLimit    int        // 核心记忆条数上限
	coreMaxChars int        // 核心记忆字符预算
	relLimit     int        // 相关记忆条数上限
	relMaxChars  int        // 相关记忆字符预算
	embedding    string     // 当前 embedding 服务名（recall.embedding 热改）
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
	s.embedding = v
	s.setMu.Unlock()
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = defaultHistoryLimit
	}
	if cfg.Embedding == "" {
		cfg.Embedding = "embedding"
	}
	s := &Server{
		cfg: cfg, logger: logger,
		historyLimit: cfg.HistoryLimit,
		coreLimit:    defaultCoreLimit,
		coreMaxChars: defaultCoreMaxChars,
		relLimit:     defaultRelLimit,
		relMaxChars:  defaultRelMaxChars,
		embedding:    cfg.Embedding,
	}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Title:       "记忆",
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "记忆服务：会话历史 + 长期记忆（remember/forget；语义召回 + bigram 兜底 + 核心注入）与上下文组装（recall 后向调用方回发 resume）",
		Provides: []protocol.Provide{
			{Action: "recall", Input: "任务上下文 {conversation_id, messages, boss}", Output: "受理回执；随后向调用方发送 resume（注入记忆后的上下文）"},
			{Action: "store", Input: "{conversation_id, role, content}", Output: "落库回执"},
			{Action: "history", Input: "{conversation_id, limit}", Output: "{conversation_id, messages:[{role, content, at}]}（按时间正序；供界面回填，不做记忆组装）"},
			{Action: "remember", Tool: true, Input: "长期记忆条目 {kind, content, priority?}（kind 如 偏好/身份/知识/事件；priority>0 = 核心记忆，每次对话都会带上）", Output: "{id, created}（同 kind+content 已有 active 条目时幂等跳过）"},
		},
		Logger: logger,
	})
	// 可设置项：预设 = 本服务的启动引导值（来源由服务自己决定）。
	s.HandleSetting("recall.history_limit", strconv.Itoa(cfg.HistoryLimit), func(v string) error {
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
	s.HandleSetting("recall.core_limit", strconv.Itoa(defaultCoreLimit), func(v string) error {
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
	s.HandleSetting("recall.core_max_chars", strconv.Itoa(defaultCoreMaxChars), func(v string) error {
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
	s.HandleSetting("recall.rel_limit", strconv.Itoa(defaultRelLimit), func(v string) error {
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
	s.HandleSetting("recall.rel_max_chars", strconv.Itoa(defaultRelMaxChars), func(v string) error {
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
	s.Handle("recall", s.actionRecall)
	s.Handle("store", s.actionStore)
	s.Handle("history", s.actionHistory)
	s.Handle("remember", s.actionRemember)
	s.Handle("forget", s.actionForget)
	// embedding 服务名：热改（空 = 关闭语义召回，静默回落 bigram）。
	s.HandleSetting("recall.embedding", cfg.Embedding, func(v string) error {
		s.setEmbeddingName(strings.TrimSpace(v))
		return nil
	}, service.Label("用哪个服务算语义向量"),
		service.Hint("关掉就退回关键词匹配——不花算力，但只认字面相近的记忆"),
		service.As("optional-service"))
	// 只读项：启动参数带起来的（换它要重启）。db 是库身份——运行中换库不做。
	s.HandleReadOnly("db", cfg.DBPath)
	s.HandleReadOnly("name", cfg.Name)
	s.HandleReadOnly("addr", cfg.Listen)
	s.HandleReadOnly("center", cfg.Center)
	return s
}

// parseNonNeg 解析非负整数（设置项校验）。
func parseNonNeg(v string) (int, error) {
	n, err := strconv.Atoi(v)
	if err != nil || n < 0 {
		return 0, fmt.Errorf("需要 ≥0 的整数，收到 %q", v)
	}
	return n, nil
}

// limits 返回当前生效的召回预算（锁内快照）。
func (s *Server) limits() (history, core, coreChars, rel, relChars int) {
	s.setMu.Lock()
	defer s.setMu.Unlock()
	return s.historyLimit, s.coreLimit, s.coreMaxChars, s.relLimit, s.relMaxChars
}

// ---------- 动作 ----------

// actionStore 记一条会话记录（对话消息幂等：与上一条完全相同时跳过）。
// Kind 为空按 Role 推断；新类型（reasoning/tool_call/tool_result）不去重也不进 LLM 上下文。
func (s *Server) actionStore(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.StorePayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if strings.TrimSpace(p.Content) == "" {
		return map[string]any{}, nil
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	if err := s.cfg.Store.Record(cid, p.Role, p.Content, p.Kind, p.Meta, p.Seq); err != nil {
		return nil, fmt.Errorf("落库失败: %w", err)
	}
	return map[string]any{}, nil
}

// actionHistory 把某会话最近的记录原样交还（界面回填用：不做记忆组装、不碰长期记忆、不改上下文）。
func (s *Server) actionHistory(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.HistoryPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	cid := p.ConversationID
	if cid == "" {
		cid = "default"
	}
	msgs, err := s.cfg.Store.History(cid, p.Limit, p.AfterSeq)
	if err != nil {
		return nil, fmt.Errorf("查询历史失败: %w", err)
	}
	return protocol.HistoryResult{ConversationID: cid, Messages: msgs}, nil
}

// actionRemember 写一条长期记忆（内容重复时幂等跳过）；来源记调用方服务名。
// 新建条目顺带算一次向量（派生数据；失败仅记日志——召回时会懒补齐）。
func (s *Server) actionRemember(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.RememberPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if s.cfg.Longterm == nil {
		return nil, errors.New("长期记忆未启用")
	}
	id, created, err := s.cfg.Longterm.Add(p.Kind, p.Content, p.Priority, p.ConversationID, env.From)
	if err != nil {
		return nil, fmt.Errorf("remember: %w", err)
	}
	if created {
		if vecs, model, err := s.embedTexts(ctx, env, []string{p.Content}, false); err != nil {
			s.logger.Printf("remember 向量化失败（召回时将懒补齐）: %v", err)
		} else if err := s.cfg.Longterm.SetEmbedding(id, vecs[0], model); err != nil {
			s.logger.Printf("remember 向量落库失败（召回时将懒补齐）: %v", err)
		}
	}
	return protocol.RememberResult{ID: id, Created: created}, nil
}

// actionForget 软删一条长期记忆（不进 provides——不给 LLM 删记忆的按钮；手动 / 直接跳转调用）。
func (s *Server) actionForget(_ context.Context, _ protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.ForgetPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	if s.cfg.Longterm == nil {
		return nil, errors.New("长期记忆未启用")
	}
	if p.ID <= 0 {
		return nil, errors.New("forget: id 必填")
	}
	ok, err := s.cfg.Longterm.Forget(p.ID)
	if err != nil {
		return nil, fmt.Errorf("forget: %w", err)
	}
	if !ok {
		return nil, fmt.Errorf("forget: 条目 %d 不存在或已删除", p.ID)
	}
	return protocol.ForgetResult{ID: p.ID}, nil
}

// recallQuery 取任务上下文里最后一条用户消息文本——作为长期记忆的检索查询。
func recallQuery(messages []protocol.Message) string {
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "user" && strings.TrimSpace(messages[i].Content) != "" {
			return messages[i].Content
		}
	}
	return ""
}

// actionRecall 是“完全启动”链的上半段：收下任务上下文 → 组装（注入记忆）→ 以 resume 发回调用方。
func (s *Server) actionRecall(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
	var p protocol.RecallPayload
	if err := protocol.DecodeRaw(in.Input, &p); err != nil {
		return nil, err
	}
	tc := p.Context
	if len(tc.Messages) == 0 {
		return nil, errors.New("recall: context.messages 为空")
	}
	cid := tc.ConversationID
	if cid == "" {
		cid = "default"
	}

	historyLimit, coreLimit, coreMaxChars, relLimit, relMaxChars := s.limits()

	history, err := s.cfg.Store.Recent(cid, historyLimit)
	if err != nil {
		return nil, fmt.Errorf("查询历史失败: %w", err)
	}
	// 去重：历史尾部与任务上下文尾部的对话消息相同时裁掉——任务链里已有的消息不重复注入。
	history = compose.TrimOverlap(history, compose.ConversationTail(tc.Messages))

	// 长期记忆：核心条目（priority>0，不看相关性）+ 相关召回（语义为主、bigram 兜底）。
	var core, relevant []string
	if s.cfg.Longterm != nil {
		entries, err := s.cfg.Longterm.ListActive()
		if err != nil {
			return nil, fmt.Errorf("查询长期记忆失败: %w", err)
		}
		coreEntries := longterm.Core(entries, coreLimit, coreMaxChars)
		coreIDs := make(map[int64]bool, len(coreEntries))
		for _, e := range coreEntries {
			coreIDs[e.ID] = true
			core = append(core, compose.EntryLine(e.Kind, e.Content))
		}
		rest := make([]longterm.Entry, 0, len(entries))
		for _, e := range entries {
			if !coreIDs[e.ID] {
				rest = append(rest, e)
			}
		}
		for _, e := range s.rankRelevant(ctx, env, rest, recallQuery(tc.Messages), relLimit, relMaxChars) {
			relevant = append(relevant, compose.EntryLine(e.Kind, e.Content))
		}
	}

	final := compose.Inject(tc.Messages, compose.BuildMemory(history, core, relevant))

	if err := s.fireResume(env, protocol.ResumePayload{Context: protocol.TaskContext{
		ConversationID: cid,
		Messages:       final,
		Boss:           tc.Boss,
	}}); err != nil {
		return nil, err
	}

	return map[string]any{"status": "accepted"}, nil
}

// rankRelevant 返回"相关记忆"条目：语义召回为主、bigram 兜底（embedding 缺失/失败/超时 → 静默回落）。
func (s *Server) rankRelevant(ctx context.Context, env protocol.Envelope, entries []longterm.Entry, query string, limit, maxChars int) []longterm.Entry {
	if strings.TrimSpace(query) == "" || len(entries) == 0 {
		return nil
	}
	if ranked, ok := s.semanticRank(ctx, env, entries, query, limit, maxChars); ok {
		return ranked
	}
	return longterm.Rank(entries, query, limit, maxChars)
}

// semanticRank：查询向量 + 条目向量（缺失/换模型后懒补齐）→ 余弦排序。
// 返回 ok=false 表示"语义召回不可用"（调用方回落 bigram）；此时不得返回部分结果。
func (s *Server) semanticRank(ctx context.Context, env protocol.Envelope, entries []longterm.Entry, query string, limit, maxChars int) ([]longterm.Entry, bool) {
	qvecs, model, err := s.embedTexts(ctx, env, []string{query}, true)
	if err != nil {
		s.logger.Printf("语义召回不可用（回落 bigram）: %v", err)
		return nil, false
	}
	if err := s.backfillEmbeddings(ctx, env, entries, model); err != nil {
		s.logger.Printf("条目向量补齐部分失败（已有向量的条目照常参与）: %v", err)
	}
	return longterm.RankSemantic(entries, qvecs[0], limit, maxChars, recallMinScore), true
}

// backfillEmbeddings 为缺失/失效（模型不一致）的条目补算向量并落库（就地更新 entries）。
// 派生数据原则：补齐失败不影响已有向量条目；失败仅记录（下次召回再试）。
func (s *Server) backfillEmbeddings(ctx context.Context, env protocol.Envelope, entries []longterm.Entry, model string) error {
	var missing []int
	for i := range entries {
		if len(entries[i].Embedding) == 0 || entries[i].EmbeddingModel != model {
			missing = append(missing, i)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	const chunk = 16
	var firstErr error
	for start := 0; start < len(missing); start += chunk {
		end := start + chunk
		if end > len(missing) {
			end = len(missing)
		}
		if end-start > s.maxBatchHint() {
			end = start + s.maxBatchHint()
		}
		idx := missing[start:end]
		texts := make([]string, 0, len(idx))
		for _, i := range idx {
			texts = append(texts, entries[i].Content)
		}
		vecs, vecModel, err := s.embedTexts(ctx, env, texts, false)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			break
		}
		for j, i := range idx {
			entries[i].Embedding = vecs[j]
			entries[i].EmbeddingModel = vecModel
			if err := s.cfg.Longterm.SetEmbedding(entries[i].ID, vecs[j], vecModel); err != nil {
				if firstErr == nil {
					firstErr = err
				}
			}
		}
	}
	return firstErr
}

// maxBatchHint 每次 embed 的批大小（embedding 服务默认上限 32；这里取保守值）。
func (s *Server) maxBatchHint() int { return 16 }

// embedTexts 调用 embedding 服务：批量文本 → 向量（限时；失败即返回错误）。
// 线路上的向量是 float64（Python 原生）；这里转成 float32 供存储（派生数据的存储格式）。
func (s *Server) embedTexts(ctx context.Context, env protocol.Envelope, texts []string, query bool) ([][]float32, string, error) {
	name := s.embeddingName()
	if name == "" {
		return nil, "", errors.New("未配置 embedding 服务")
	}
	cctx, cancel := context.WithTimeout(ctx, embedTimeout)
	defer cancel()
	data, err := s.Call(cctx, name, "embed", protocol.EmbedPayload{Texts: texts, Query: query}, env.Trace, s.cfg.Name)
	if err != nil {
		return nil, "", err
	}
	var res protocol.EmbedResult
	if err := protocol.DecodeRaw(data, &res); err != nil {
		return nil, "", fmt.Errorf("embed 结果解析失败: %w", err)
	}
	if len(res.Vectors) != len(texts) {
		return nil, "", fmt.Errorf("embed 返回向量数不符: %d != %d", len(res.Vectors), len(texts))
	}
	vecs := make([][]float32, len(res.Vectors))
	for i, v := range res.Vectors {
		out := make([]float32, len(v))
		for j, f := range v {
			out[j] = float32(f)
		}
		vecs[i] = out
	}
	return vecs, res.Model, nil
}

// fireResume 向调用方发送组装完成的上下文——记忆往返期间 boss 是调用方。
// resolve 失败会同步返回（调用方可收到错误）；投递本身异步进行（发出即完）。
func (s *Server) fireResume(req protocol.Envelope, payload any) error {
	target := req.From
	if target == "" {
		return errors.New("recall: 请求缺少 from，无法回发 resume")
	}
	if _, _, err := s.Resolve(target); err != nil {
		return err
	}
	s.Fire(target, "resume", payload, req.Trace, target)
	return nil
}
