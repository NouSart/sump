package server

// memory 门面：唯一注册到设置中心的进程。
// 对外 method 原样透传给 session / experience；context 在本地用两跳拼装。
// 门面不碰任何数据库，也不 import 会话/经验的数据包。

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/NouSart/sump/memory/compose"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	"github.com/vmihailenco/msgpack/v5"
)

// contextBaseResult 是 session.context_base 的回执：只含会话侧原始材料。
type contextBaseResult struct {
	Task    protocol.Task      `msgpack:"task"`
	Work    spv2.Work          `msgpack:"work"`
	History []protocol.Message `msgpack:"history"`
}

// injectRequest 是 experience.inject 的请求：viewer 预留给读取侧可见性过滤。
type injectRequest struct {
	Viewer string `msgpack:"viewer,omitempty"`
	Origin string `msgpack:"origin,omitempty"`
	Query  string `msgpack:"query,omitempty"`
}

// injectResult 是 experience 负责的四节记忆（不含最近对话）。
type injectResult struct {
	Core       []string `msgpack:"core,omitempty"`
	Relevant   []string `msgpack:"relevant,omitempty"`
	Experience []string `msgpack:"experience,omitempty"`
	Others     []string `msgpack:"others,omitempty"`
}

// registerFacadeProxies 把数据归属明确的方法换成透传处理器。
// 名片 schema / Tool 标记沿用原注册，避免门面与内部服务的对外描述漂移。
func (s *Server) registerFacadeProxies() {
	if !s.role.isFacade() {
		return
	}
	for name := range sessionMethods {
		if internalMethods[name] {
			continue
		}
		if !s.ProxyNative(name, s.nativeForward("session", name)) {
			s.logger.Printf("门面透传注册失败（session.%s 未注册）", name)
		}
	}
	for name := range experienceMethods {
		if name == "inject" {
			continue
		}
		if !s.ProxyNative(name, s.nativeForward("experience", name)) {
			s.logger.Printf("门面透传注册失败（experience.%s 未注册）", name)
		}
	}
}

// nativeForward 把入参 body 与帧头 from/work 原样交给内部服务。
func (s *Server) nativeForward(peer, method string) func(context.Context, *spv2.Call) error {
	return func(ctx context.Context, call *spv2.Call) error {
		raw, err := s.CallWithRef(ctx, call.Frame.From, peer, method,
			msgpack.RawMessage(call.Frame.Body), call.Frame.Work)
		if err != nil {
			return replyForwardError(call, peer, method, err)
		}
		return call.Reply(msgpack.RawMessage(raw))
	}
}

// replyForwardError 保留内部服务的错误码；连不上时统一报 unavailable。
func replyForwardError(call *spv2.Call, peer, method string, err error) error {
	var e *spv2.Error
	if errors.As(err, &e) {
		return call.ReplyError(e.Code, e.Message, e.Retryable, e.Details)
	}
	return call.ReplyError(spv2.CodeUnavailable,
		fmt.Sprintf("%s.%s 不可用: %v", peer, method, err), true, nil)
}

// nativeContextFacade 是 context 的门面实现：会话材料 + 记忆四节，本地拼装。
func (s *Server) nativeContextFacade(ctx context.Context, call *spv2.Call) error {
	var p protocol.ContextPayload
	if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "context body 无效: "+err.Error(), false, nil)
	}
	from := call.Frame.From
	if from.Kind != "agent" && from.Kind != "service" &&
		from.Authority != spv2.AuthorityOwner && from.Authority != spv2.AuthorityRuntime {
		return call.ReplyError(spv2.CodePermissionDenied, "context: from.kind 必须是 agent/service（或 owner/runtime）", false, nil)
	}
	if len(p.Messages) == 0 {
		return call.ReplyError(spv2.CodeInvalidArgument, "context: context.messages 为空", false, nil)
	}

	baseRaw, err := s.CallWithRef(ctx, from, "session", "context_base",
		msgpack.RawMessage(call.Frame.Body), call.Frame.Work)
	if err != nil {
		return replyForwardError(call, "session", "context_base", err)
	}
	var base contextBaseResult
	if err := protocol.DecodeRaw(baseRaw, &base); err != nil {
		return call.ReplyError(spv2.CodeInternal, "context_base 回执无法解码: "+err.Error(), true, nil)
	}

	cid := p.Conversation
	if cid == "" {
		cid = "default"
	}
	injectRaw, err := s.CallWithRef(ctx, from, "experience", "inject", injectRequest{
		Viewer: from.ID,
		Origin: cid,
		Query:  recallQuery(p.Messages),
	}, call.Frame.Work)
	if err != nil {
		return replyForwardError(call, "experience", "inject", err)
	}
	var injected injectResult
	if err := protocol.DecodeRaw(injectRaw, &injected); err != nil {
		return call.ReplyError(spv2.CodeInternal, "inject 回执无法解码: "+err.Error(), true, nil)
	}

	history := compose.TrimOverlap(base.History, compose.ConversationTail(p.Messages))
	final := compose.Inject(p.Messages, compose.BuildMemory(history,
		injected.Core, injected.Relevant, injected.Experience, injected.Others))
	return call.Reply(map[string]any{"messages": final, "work": base.Work, "task": base.Task})
}

// forwardSetting 把设置下发转给数据归属方；内部服务走自己的 configure 校验。
func (s *Server) forwardSetting(peer, key, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := s.Call(ctx, peer, "configure", protocol.ConfigurePayload{Key: key, Value: value}, "")
	if err != nil {
		return fmt.Errorf("转发设置 %s 到 %s 失败: %w", key, peer, err)
	}
	return nil
}
