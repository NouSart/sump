package service

// SPv2.1 传输桥：让现有 service.Service 同时说 SPv2.1。
//
// 业务 handler 仍是 v1 形状（protocol.JumpPayload），这里只做 method/body 映射：
//   - 入站：/spv2 的 call -> 合成 v1 jump -> 调已有 actions[method] -> 回 reply
//   - 出站：Service.Call 优先走目标 /spv2；dial 失败（目标只支持 v1）才回退 v1
//
// 这不是“业务已迁移”，而是“传输层先切”；原生 method/卡片后续按服务替换。

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// spv2DialError 标记“目标 /spv2 不可达”，只有这种错误允许回退 v1。
type spv2DialError struct{ err error }

func (e *spv2DialError) Error() string { return "spv2 目标不可达: " + e.err.Error() }
func (e *spv2DialError) Unwrap() error { return e.err }

func genericSchema() map[string]any {
	return map[string]any{"type": "object"}
}

// buildSPv2 根据已注册 actions 与服务声明生成 SPv2 服务端。
func (s *Service) buildSPv2() error {
	// configure 是骨架保留的设置下发方法；原生 /spv2 也直接解 body，不合成 v1。
	if _, ok := s.native["configure"]; !ok {
		s.native["configure"] = NativeMethod{
			Method: spv2.Method{
				Name: "configure", Shape: spv2.ShapeCall, Version: 1,
				Input: genericSchema(), Output: genericSchema(),
				Description: "应用设置中心下发的单个设置项",
			},
			Handler: s.nativeConfigure,
		}
	}
	provides := map[string]protocol.Provide{}
	for _, p := range s.cfg.Provides {
		provides[p.Action] = p
	}
	methods := make([]spv2.Method, 0, len(s.actions)+len(s.native)+3)
	seen := map[string]bool{}
	nativeNames := make([]string, 0, len(s.native))
	for name := range s.native {
		nativeNames = append(nativeNames, name)
	}
	sort.Strings(nativeNames)
	for _, name := range nativeNames {
		m := s.native[name].Method
		if m.TimeoutMS == 0 {
			m.TimeoutMS = s.cfg.ActionTimeout.Milliseconds()
		}
		methods = append(methods, m)
		seen[name] = true
	}
	for action := range s.actions {
		if seen[action] {
			continue
		}
		p := provides[action]
		desc := p.Input
		if p.Output != "" {
			desc += " -> " + p.Output
		}
		methods = append(methods, spv2.Method{
			Name:        action,
			Shape:       spv2.ShapeCall,
			Version:     1,
			Tool:        p.Tool,
			LongRunning: p.Async,
			TimeoutMS:   s.cfg.ActionTimeout.Milliseconds(),
			Description: desc,
			Input:       genericSchema(),
			Output:      genericSchema(),
		})
		seen[action] = true
	}
	for _, name := range []string{"hello", "health", "metrics"} {
		if seen[name] {
			continue
		}
		methods = append(methods, spv2.Method{
			Name: name, Shape: spv2.ShapeCall, Version: 1,
			TimeoutMS: s.cfg.ActionTimeout.Milliseconds(),
			Input:     genericSchema(), Output: genericSchema(),
		})
	}
	maxBody := s.cfg.MaxMessageBytes
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	card := spv2.Card{
		Protocol:      spv2.ProtocolVersion,
		Name:          s.cfg.Name,
		Actor:         spv2.ServiceRef(s.cfg.Name),
		Version:       "spv2-native/0.1",
		Description:   s.cfg.Description,
		Capabilities:  []string{"msgpack", "json"},
		Methods:       methods,
		Limits:        spv2.Limits{MaxBody: maxBody},
		Observability: spv2.Observability{Health: true, Metrics: true, Format: "spv2.metrics.v1"},
	}
	srv, err := spv2.NewServer(card, spv2.WithMaxMessageBytes(maxBody))
	if err != nil {
		return err
	}
	for _, name := range nativeNames {
		srv.Handle(name, spv2.Handler(s.native[name].Handler))
	}
	for action := range s.actions {
		if _, ok := s.native[action]; ok {
			continue
		}
		action := action
		h := s.actions[action]
		srv.Handle(action, func(ctx context.Context, call *spv2.Call) error {
			raw, err := protocol.EncodePayload(protocol.JumpPayload{Action: action, Input: call.Frame.Body})
			if err != nil {
				return spv2.ErrCode(spv2.CodeInternal, "构造 v1 payload 失败: "+err.Error(), false, nil)
			}
			env := protocol.Envelope{
				V: protocol.ProtocolVersion, ID: call.Frame.ID, Trace: call.Frame.Work,
				Boss: call.Frame.From.ID, Type: protocol.TypeJump,
				From: call.Frame.From.ID, To: call.Frame.To.ID, Payload: raw,
			}
			out, err := h(ctx, env, protocol.JumpPayload{Action: action, Input: call.Frame.Body})
			if err != nil {
				return spv2.ErrCode(spv2.CodeInternal, err.Error(), false, nil)
			}
			return call.Reply(out)
		})
	}
	s.spv2Srv = srv
	return nil
}

// callSPv2 向目标 /spv2 发一跳；只有 dial 失败会包装成 spv2DialError。
func (s *Service) callSPv2(ctx context.Context, name, addr, action string, raw msgpack.RawMessage, trace string) (msgpack.RawMessage, error) {
	s.spv2PeerMu.Lock()
	known, seen := s.spv2Peers[addr]
	s.spv2PeerMu.Unlock()
	if seen && !known {
		return nil, &spv2DialError{err: errors.New("已知 v1 peer")}
	}
	target := spv2Addr(addr)
	c, err := spv2.Dial(ctx, target, spv2.WithClientMaxMessageBytes(s.cfg.MaxMessageBytes))
	if err != nil {
		s.markSPv2Peer(addr, false)
		return nil, &spv2DialError{err: err}
	}
	defer c.Close()
	if !seen || !known {
		helloCtx, cancel := context.WithTimeout(ctx, time.Second)
		_, helloErr := c.Hello(helloCtx, spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef(name))
		cancel()
		if helloErr != nil {
			s.markSPv2Peer(addr, false)
			return nil, &spv2DialError{err: helloErr}
		}
		s.markSPv2Peer(addr, true)
	}
	work := trace
	if work == "" {
		work = spv2.NewID()
	}
	f, err := spv2.NewCall(work, spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef(name), action, []byte(raw))
	if err != nil {
		return nil, err
	}
	resp, err := c.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return msgpack.RawMessage(resp.Body), nil
}

func (s *Service) markSPv2Peer(addr string, ok bool) {
	s.spv2PeerMu.Lock()
	s.spv2Peers[addr] = ok
	s.spv2PeerMu.Unlock()
}

func spv2Addr(addr string) string {
	if strings.HasSuffix(addr, "/ws") {
		return strings.TrimSuffix(addr, "/ws") + "/spv2"
	}
	if strings.HasSuffix(addr, "/spv2") {
		return addr
	}
	return strings.TrimSuffix(addr, "/") + "/spv2"
}
