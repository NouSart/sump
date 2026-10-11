// memory 服务的核心：把记忆动作注册到服务骨架（注册 / 心跳 / 名册 /
// 断线自愈 / 告别见 service 包）。
// recall 收到调用方托管的任务上下文 → 查会话历史并组装记忆 → 以 resume 发回调用方（触发其“完全启动”）。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
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
	HistoryLimit      int             // recall 返回的最近消息条数（默认 12）
}

// v0 注入预算（对齐 v1 量级；以后可升为设置项）。
const (
	coreLimit    = 3   // 核心记忆条数上限
	coreMaxChars = 400 // 核心记忆字符预算
	relLimit     = 5   // 相关记忆条数上限
	relMaxChars  = 800 // 相关记忆字符预算
)

// Server 是 memory 服务实例（服务骨架 + 会话库）。
type Server struct {
	*service.Service

	cfg    Config
	logger *log.Logger
}

// New 创建实例并注册动作。
func New(cfg Config, logger *log.Logger) *Server {
	if cfg.HistoryLimit <= 0 {
		cfg.HistoryLimit = 12
	}
	s := &Server{cfg: cfg, logger: logger}
	s.Service = service.New(service.Config{
		Name:              cfg.Name,
		Listen:            cfg.Listen,
		Center:            cfg.Center,
		HeartbeatInterval: cfg.HeartbeatInterval,
		Description:       "记忆服务：会话历史 + 长期记忆（remember/forget；bigram 召回 + 核心注入）与上下文组装（recall 后向调用方回发 resume）",
		Provides: []protocol.Provide{
			{Action: "recall", Input: "任务上下文 {conversation_id, messages, boss}", Output: "受理回执；随后向调用方发送 resume（注入记忆后的上下文）"},
			{Action: "store", Input: "{conversation_id, role, content}", Output: "落库回执"},
			{Action: "remember", Tool: true, Input: "长期记忆条目 {kind, content, priority?}（kind 如 偏好/身份/知识/事件；priority>0 = 核心记忆，每次对话都会带上）", Output: "{id, created}（同 kind+content 已有 active 条目时幂等跳过）"},
		},
		Logger: logger,
	})
	s.Handle("recall", s.actionRecall)
	s.Handle("store", s.actionStore)
	s.Handle("remember", s.actionRemember)
	s.Handle("forget", s.actionForget)
	return s
}

// ---------- 动作 ----------

// actionStore 记一条对话消息（幂等：与上一条完全相同时跳过）。
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
	if err := s.cfg.Store.Append(cid, p.Role, p.Content); err != nil {
		return nil, fmt.Errorf("落库失败: %w", err)
	}
	return map[string]any{}, nil
}

// actionRemember 写一条长期记忆（内容重复时幂等跳过）；来源记调用方服务名。
func (s *Server) actionRemember(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
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
func (s *Server) actionRecall(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
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

	history, err := s.cfg.Store.Recent(cid, s.cfg.HistoryLimit)
	if err != nil {
		return nil, fmt.Errorf("查询历史失败: %w", err)
	}
	// 去重：历史尾部与任务上下文尾部的对话消息相同时裁掉——任务链里已有的消息不重复注入。
	history = compose.TrimOverlap(history, compose.ConversationTail(tc.Messages))

	// 长期记忆：核心条目（priority>0，不看相关性）+ bigram 相关召回（以最新用户消息为查询）。
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
		for _, e := range longterm.Rank(rest, recallQuery(tc.Messages), relLimit, relMaxChars) {
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
