package service

// SPv2.1 原生控制面客户端：register / heartbeat / roster / settings 走 /spv2。
// v1 仍保留为兼容边缘；原生注册失败时 connectAndRegister 会自动回退。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func (s *Service) connectAndRegisterSPv2(ctx context.Context) error {
	target := spv2Addr(s.cfg.Center)
	c, err := spv2.Dial(ctx, target,
		spv2.WithClientMaxMessageBytes(s.cfg.MaxMessageBytes),
		spv2.WithOnEvent(s.onNativeCenterEvent),
	)
	if err != nil {
		return err
	}
	reg := protocol.RegisterPayload{
		Name:        s.cfg.Name,
		Addr:        s.selfURL,
		Title:       s.cfg.Title,
		Description: s.cfg.Description,
		Provides:    s.cfg.Provides,
		Settings:    s.declarations(),
		HeartbeatMS: s.cfg.HeartbeatInterval.Milliseconds(),
	}
	frame, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef("settings-center"), "register", reg)
	if err != nil {
		_ = c.Close()
		return err
	}
	rctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	resp, err := c.Call(rctx, frame)
	cancel()
	if err != nil {
		_ = c.Close()
		return fmt.Errorf("spv2 register: %w", err)
	}
	var roster protocol.RosterPayload
	if err := protocol.DecodeRaw(resp.Body, &roster); err != nil {
		_ = c.Close()
		return err
	}

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		_ = c.Close()
		return fmt.Errorf("服务已关闭")
	}
	s.centerNative = c
	s.regs++
	regs := s.regs
	s.mu.Unlock()
	s.setRoster(roster)
	if regs == 1 {
		s.logger.Printf("%s 已接入设置中心（SPv2 原生，名册 %d 个服务, rev %d）", s.cfg.Name, len(roster.Services), roster.Revision)
	} else {
		s.logger.Printf("%s 已重新接入设置中心（SPv2 原生，第 %d 次注册）", s.cfg.Name, regs)
	}

	loopCtx := ctx
	s.mu.Lock()
	if s.lifeCtx != nil {
		loopCtx = s.lifeCtx
	}
	s.mu.Unlock()
	s.goBackground(func() { s.refreshSettings(loopCtx) })
	s.goBackground(func() { s.heartbeatLoopNative(loopCtx, c) })
	return nil
}

func (s *Service) heartbeatLoopNative(ctx context.Context, c *spv2.Client) {
	t := time.NewTicker(s.cfg.HeartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.Done():
			return
		case <-t.C:
			ev, err := spv2.NewEvent(spv2.NewID(), spv2.ServiceRef(s.cfg.Name), spv2.ServiceRef("settings-center"), "heartbeat",
				protocol.HeartbeatPayload{Status: "ok"})
			if err != nil {
				continue
			}
			if err := c.Send(ev); err != nil {
				return
			}
		}
	}
}

func (s *Service) onNativeCenterEvent(f spv2.Frame) {
	if f.Type != spv2.TypeEvent {
		return
	}
	switch f.Method {
	case "roster":
		var r protocol.RosterPayload
		if err := protocol.DecodeRaw(f.Body, &r); err != nil {
			s.logger.Printf("SPv2 roster 解析失败: %v", err)
			return
		}
		s.setRoster(r)
		s.logger.Printf("roster 更新（SPv2）: %d 个服务 (rev %d)", len(r.Services), r.Revision)
	case "settings":
		// 与 v1 一致：服务侧设置由 configure 直达；事件只给面板观察者。
	default:
		s.logger.Printf("未知 SPv2 事件: %s", f.Method)
	}
}
