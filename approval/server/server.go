// Package server 负责 SUMP 消息适配、配置与自身故障上报，不承担命令执行。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QAQ-awa-QAQ/sump/approval/engine"
	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

type Config struct {
	Name, Listen, Center, PolicyJSON string
	Engine                           *engine.Engine
	Concurrency                      int
	Logger                           *log.Logger

	// Portal 是能裁决的门户服务名（默认 frontend）；空 = 第二层关闭（只做第一层，review 原样返回）。
	Portal string
	// ReviewTTL 是一条待审命令最多挂多久（默认 10 分钟，与 shell 的 approval.timeout_ms 对齐）。
	ReviewTTL time.Duration
	// MaxPending 是同时挂起的上限（默认 32）；满了按 review 直接返回，不排队。
	MaxPending int
}

type Server struct {
	*service.Service
	engine   *engine.Engine
	slots    chan struct{}
	accepted atomic.Uint64
	busy     atomic.Uint64
	logger   *log.Logger

	cfgMu      sync.Mutex
	portal     string
	reviewTTL  time.Duration
	maxPending int

	pendingMu sync.Mutex
	pending   map[string]*pendingEntry

	// portalReady 判断"现在有人能裁决吗"。默认看名册；测试可替换。
	portalReady func() bool
}

func New(cfg Config) (*Server, error) {
	if cfg.Engine == nil {
		return nil, errors.New("缺少审批引擎")
	}
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = 8
	}
	if cfg.Concurrency > 64 {
		return nil, errors.New("审批并发不能超过 64")
	}
	if cfg.Name == "" {
		cfg.Name = "approval"
	}
	if cfg.ReviewTTL <= 0 {
		cfg.ReviewTTL = 10 * time.Minute
	}
	if cfg.ReviewTTL > 30*time.Minute {
		return nil, errors.New("待审时长不能超过 30 分钟")
	}
	if cfg.MaxPending <= 0 {
		cfg.MaxPending = 32
	}
	if cfg.MaxPending > 256 {
		return nil, errors.New("待审队列上限不能超过 256")
	}
	s := &Server{
		engine:     cfg.Engine,
		slots:      make(chan struct{}, cfg.Concurrency),
		portal:     cfg.Portal,
		reviewTTL:  cfg.ReviewTTL,
		maxPending: cfg.MaxPending,
		pending:    make(map[string]*pendingEntry),
		logger:     cfg.Logger,
	}
	if s.logger == nil {
		s.logger = log.Default()
	}
	s.portalReady = func() bool { return s.portalInRoster(s.portalNow()) }
	// 挂起要活得比单次动作超时更久：ActionTimeout 必须覆盖 TTL（否则用户还没点，上下文先被掐）。
	actionTimeout := 8 * time.Second
	if cfg.Portal != "" {
		actionTimeout = cfg.ReviewTTL + 30*time.Second
	}
	s.Service = service.New(service.Config{Name: cfg.Name, Title: "审批", Listen: cfg.Listen, Center: cfg.Center,
		ActionTimeout: actionTimeout, Logger: cfg.Logger,
		Description: "终端命令处理与审批：返回终端裁决和 LLM 反馈，由 shell 补报到模型；白名单直过，其余挂起等门户用户裁决",
		Provides: []protocol.Provide{
			{Action: protocol.ApprovalAction, Input: "{schema_version,item_id,plan}", Output: "{decision,reason_code,llm_feedback,plan_digest,policy_version}"},
			{Action: protocol.ApprovalDecideAction, Input: "{item_id,decision(allow|deny),note?}", Output: "{status,item_id,decision}（仅门户可调）"},
			{Action: protocol.ApprovalListAction, Input: "{}", Output: "{items:[{item_id,command,work_dir,reason_code,summary,expires_ms}]}（仅门户可调）"},
			{Action: "capabilities", Input: "{}", Output: "{dialects,other_dialects,review_*}（能自动分析的方言、其它方言的处理方式、第二层状态）"},
		},
	})
	s.HandleSetting("policy.json", cfg.PolicyJSON, s.engine.UpdatePolicy,
		service.Label("命令审批策略"),
		service.Hint("JSON 策略：deny_commands 里的命令直接拒绝；rules 里列出的命令免审批直过；其余挂起等你在门户点批准"),
		service.As("textarea"))
	s.Handle(protocol.ApprovalAction, s.approve)
	s.Handle(protocol.ApprovalDecideAction, s.decide)
	s.Handle(protocol.ApprovalListAction, s.list)
	s.Handle("capabilities", func(context.Context, protocol.Envelope, protocol.JumpPayload) (any, error) {
		// dialects 只列**能自动分析**的方言；别的方言不拒也不跳过审批，而是逐条转第二层
		// （other_dialects 说明这一点，免得看的人以为"不支持 = 不审批"）。
		return map[string]any{"schema_version": 1, "dialects": []string{"powershell"}, "profile": protocol.PowerShellProfile,
			"other_dialects": "review",
			"feedback_route": "shell.report_result", "review_enabled": s.portalNow() != "",
			"review_portal": s.portalNow(), "review_ttl_ms": s.pendingTTLNow().Milliseconds(),
			"pending": len(s.pendingSnapshot()), "accepted": s.accepted.Load(), "busy": s.busy.Load()}, nil
	})
	return s, nil
}

func (s *Server) approve(ctx context.Context, env protocol.Envelope, in protocol.JumpPayload) (out any, err error) {
	// handler 级限制不等于传输层大帧限制；部署边界保持为可信本地服务。
	if len(in.Input) > 64<<10 {
		return nil, errors.New("审批载荷超过 64 KiB")
	}
	var req protocol.CommandApprovalRequest
	if err := protocol.DecodeRaw(in.Input, &req); err != nil {
		return nil, errors.New("审批载荷无效")
	}
	select {
	case s.slots <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	default:
		s.busy.Add(1)
		return nil, errors.New("approval_busy: 审批服务繁忙，本次未受理")
	}
	// 并发槽只约束"解析+裁决"这段 CPU 活；用户裁决的等待不占槽（否则 8 条挂起就把服务占满）。
	released := false
	release := func() {
		if !released {
			released = true
			<-s.slots
		}
	}
	defer func() {
		release()
		if r := recover(); r != nil {
			s.Report("approval_internal", "审批处理发生内部异常；本次未批准")
			out, err = nil, errors.New("审批内部异常，本次未批准")
		}
	}()
	result, err := s.engine.Evaluate(ctx, req)
	if err != nil {
		// 输入错误在执行解析前检查；解析组件故障与正常策略拒绝分开。
		if result.DecisionID != "" && ctx.Err() == nil {
			s.Report("approval_parser", "命令解析组件失败；不含原命令："+fmt.Sprintf("%T", err))
		}
		return nil, err
	}
	s.accepted.Add(1)
	release()
	if result.Decision == protocol.DecisionReview {
		return s.awaitUserDecision(ctx, env, result, req)
	}
	return result, nil
}
