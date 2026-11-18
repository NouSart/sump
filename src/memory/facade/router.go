package facade

// 门面到内部服务的只读探测：卡片、设置声明、健康。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func (s *Server) peerCard(ctx context.Context, peer string) (spv2.Card, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_, addr, err := s.Resolve(peer)
	if err != nil {
		return spv2.Card{}, err
	}
	conn, err := spv2.Dial(cctx, addr)
	if err != nil {
		return spv2.Card{}, err
	}
	defer conn.Close()
	frame, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef(peer), "hello", nil)
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

func (s *Server) peerSettings(ctx context.Context, peer string) ([]protocol.Setting, error) {
	raw, err := s.Call(ctx, peer, "settings_schema", nil, "")
	if err != nil {
		return nil, err
	}
	var out []protocol.Setting
	if err := protocol.DecodeRaw(raw, &out); err != nil {
		return nil, fmt.Errorf("settings_schema 回执无法解码: %w", err)
	}
	return out, nil
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
