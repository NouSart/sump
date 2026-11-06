package service

// SPv2.1 原生控制面客户端：register / heartbeat / roster / settings 走 /spv2。

import (
	"context"
	"fmt"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// connectAndRegister 建立一条中心连接、注册并启动心跳。
func (s *Service) connectAndRegister(ctx context.Context) error {
	target := spv2Addr(s.cfg.Center)
	c, err := spv2.Dial(ctx, target,
		spv2.WithClientMaxMessageBytes(s.maxMessageBytes()),
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
		s.logger.Printf("%s 已接入设置中心（名册 %d 个服务, rev %d）", s.cfg.Name, len(roster.Services), roster.Revision)
	} else {
		s.logger.Printf("%s 已重新接入设置中心（第 %d 次注册）", s.cfg.Name, regs)
	}

	loopCtx := ctx
	s.mu.Lock()
	if s.lifeCtx != nil {
		loopCtx = s.lifeCtx
	}
	s.mu.Unlock()
	s.goBackground(func() { s.refreshSettings(loopCtx) })
	s.goBackground(func() { s.heartbeatLoop(loopCtx, c) })
	return nil
}

// watchCenter 监视中心连接：断开后重连并重新注册（指数退避，上限 30s）。
func (s *Service) watchCenter(ctx context.Context) {
	const maxBackoff = 30 * time.Second
	backoff := time.Second
	for {
		s.mu.Lock()
		closed := s.closed
		n := s.centerNative
		s.mu.Unlock()
		if closed || ctx.Err() != nil {
			return
		}

		if n == nil {
			if err := s.connectAndRegister(ctx); err != nil {
				if ctx.Err() != nil || s.isClosed() {
					return
				}
				s.logger.Printf("%s 重连设置中心失败: %v（%v 后重试）", s.cfg.Name, err, backoff)
				select {
				case <-time.After(backoff):
				case <-ctx.Done():
					return
				}
				backoff *= 2
				if backoff > maxBackoff {
					backoff = maxBackoff
				}
				continue
			}
			backoff = time.Second
			continue
		}

		select {
		case <-ctx.Done():
			return
		case <-n.Done():
			// 连接断开：清空后快速重连（首试 1s，不做长退避）。
			// Shutdown 也会关连接；先看 closed，避免"关闭后还打一行重连日志再等 1s"。
			s.mu.Lock()
			if s.centerNative == n {
				s.centerNative = nil
			}
			closed := s.closed
			s.mu.Unlock()
			if closed {
				return
			}
			s.logger.Printf("%s 与设置中心连接断开，1s 后重连", s.cfg.Name)
			select {
			case <-time.After(time.Second):
			case <-ctx.Done():
				return
			}
		}
	}
}

// heartbeatLoop 向中心发心跳；写失败即关闭连接（触发自愈）。
func (s *Service) heartbeatLoop(ctx context.Context, c *spv2.Client) {
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
			s.logger.Printf("roster 解析失败: %v", err)
			return
		}
		s.setRoster(r)
		s.logger.Printf("roster 更新: %d 个服务 (rev %d)", len(r.Services), r.Revision)
	case "settings":
		// 设置事件是给面板等观察者的信息流；服务侧的设置变更由 configure 直达——这里静默忽略。
	default:
		s.logger.Printf("未知事件: %s", f.Method)
	}
}
