package server

// 门面启动自检：内部服务必须在、方法集不能冲突。
// 第一版保持硬门：连不上或冲突就启动失败，不带病对外。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) bootstrapPeers(ctx context.Context) error {
	cards := map[string]spv2.Card{}
	for _, name := range []string{"session", "experience"} {
		card, err := s.peerCard(ctx, name)
		if err != nil {
			return fmt.Errorf("门面自检: %s 不可用: %w", name, err)
		}
		if card.Name != name {
			return fmt.Errorf("门面自检: 对端 %s 自报名 %s 不一致", name, card.Name)
		}
		cards[name] = card
		s.AddHealthCheck(s.peerHealth(name))
	}
	seen := map[string]string{}
	for _, name := range []string{"session", "experience"} {
		for _, m := range cards[name].Methods {
			if isControlMethod(m.Name) {
				continue
			}
			if owner, ok := seen[m.Name]; ok {
				return fmt.Errorf("门面自检: method %s 同时由 %s 与 %s 声明", m.Name, owner, name)
			}
			seen[m.Name] = name
		}
	}
	// 每个域内 method 必须能路由；缺一个就说明内部服务裁剪错了。
	for method := range sessionMethods {
		if internalMethods[method] {
			continue
		}
		if seen[method] != "session" {
			return fmt.Errorf("门面自检: session 未声明 method %s", method)
		}
	}
	for method := range experienceMethods {
		if internalMethods[method] {
			continue
		}
		if seen[method] != "experience" {
			return fmt.Errorf("门面自检: experience 未声明 method %s", method)
		}
	}
	return nil
}

func isControlMethod(name string) bool {
	switch name {
	case "hello", "health", "metrics", "configure":
		return true
	default:
		return false
	}
}

func (s *Server) peerCard(ctx context.Context, peer string) (spv2.Card, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	name, addr, err := s.Resolve(peer)
	if err != nil {
		return spv2.Card{}, err
	}
	conn, err := spv2.Dial(cctx, addr)
	if err != nil {
		return spv2.Card{}, err
	}
	defer conn.Close()
	frame, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef(name), "hello", nil)
	if err != nil {
		return spv2.Card{}, err
	}
	resp, err := conn.Call(cctx, frame)
	if err != nil {
		return spv2.Card{}, err
	}
	var hello spv2.HelloResponse
	if err := protocol.DecodeRaw(resp.Body, &hello); err != nil {
		return spv2.Card{}, err
	}
	return hello.Card, nil
}

func (s *Server) peerHealth(peer string) spv2.HealthChecker {
	return func(ctx context.Context) spv2.HealthCheck {
		raw, err := s.Call(ctx, peer, "health", nil, "")
		if err != nil {
			return spv2.HealthCheck{Name: peer, Status: spv2.HealthFail}
		}
		var res spv2.HealthResponse
		if err := protocol.DecodeRaw(raw, &res); err != nil {
			return spv2.HealthCheck{Name: peer, Status: spv2.HealthFail}
		}
		return spv2.HealthCheck{Name: peer, Status: res.Status}
	}
}
