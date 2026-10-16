// Package server 负责 SUMP 消息适配、配置与自身故障上报，不承担命令执行。
package server

import (
	"context"
	"errors"
	"fmt"
	"log"
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
}

type Server struct {
	*service.Service
	engine   *engine.Engine
	slots    chan struct{}
	accepted atomic.Uint64
	busy     atomic.Uint64
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
	s := &Server{engine: cfg.Engine, slots: make(chan struct{}, cfg.Concurrency)}
	s.Service = service.New(service.Config{Name: cfg.Name, Listen: cfg.Listen, Center: cfg.Center,
		ActionTimeout: 8 * time.Second, Logger: cfg.Logger,
		Description: "终端命令处理与审批：返回终端裁决和 LLM 反馈，由 shell 补报到模型；第一层",
		Provides: []protocol.Provide{
			{Action: protocol.ApprovalAction, Input: "{schema_version,item_id,plan}", Output: "{decision,reason_code,llm_feedback,plan_digest,policy_version}"},
			{Action: "capabilities", Input: "{}", Output: "支持范围与第二层状态"},
		},
	})
	s.HandleSetting("policy.json", cfg.PolicyJSON, s.engine.UpdatePolicy)
	s.Handle(protocol.ApprovalAction, s.approve)
	s.Handle("capabilities", func(context.Context, protocol.Envelope, protocol.JumpPayload) (any, error) {
		return map[string]any{"schema_version": 1, "dialects": []string{"powershell"}, "profile": protocol.PowerShellProfile,
			"feedback_route": "shell.report_result", "review_enabled": false, "accepted": s.accepted.Load(), "busy": s.busy.Load()}, nil
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
		defer func() { <-s.slots }()
	default:
		s.busy.Add(1)
		return nil, errors.New("approval_busy: 审批服务繁忙，本次未受理")
	}
	defer func() {
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
	return result, nil
}
