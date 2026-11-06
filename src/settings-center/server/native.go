package server

// settings-center 的原生 SPv2.1 控制面。
//
// 这是引导面，不是业务面：register / heartbeat / report 仍复用 hub 与 settings 的
// 同一套逻辑；中心广播的 roster / settings 直接以 SPv2 event 下发。

import (
	"context"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

type nativeConn struct {
	call *spv2.Call
	name string
}

func (c *nativeConn) SendEvent(typ string, payload any) error {
	return c.call.SendEvent(typ, payload)
}

func (c *nativeConn) Close() error { return c.call.Close() }

func newNativeServer(s *Server) (*spv2.Server, error) {
	methods := []spv2.Method{
		{Name: "register", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema(), Description: "注册服务并返回名册"},
		{Name: "heartbeat", Shape: spv2.ShapeEvent, Version: 1, Input: objectSchema(), Output: objectSchema(), Description: "心跳 / bye"},
		{Name: "report", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema(), Description: "错误上报"},
		{Name: "ping", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema()},
		{Name: "roster", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema()},
		{Name: "list_settings", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema()},
		{Name: "set_setting", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema()},
		{Name: "reset_setting", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema()},
		{Name: "report_setting", Shape: spv2.ShapeCall, Version: 1, Input: objectSchema(), Output: objectSchema()},
	}
	card := spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: selfServiceName, Actor: spv2.ServiceRef(selfServiceName),
		Version: "settings-center-native/0.1", Description: "服务注册 · 全局信息 · 设置与调度",
		Methods: methods, Limits: spv2.Limits{MaxBody: 1 << 20},
		Observability: spv2.Observability{Health: true, Metrics: true, Format: "spv2.metrics.v1"},
	}
	srv, err := spv2.NewServer(card, spv2.WithMaxMessageBytes(1<<20))
	if err != nil {
		return nil, err
	}
	srv.Handle("register", s.nativeRegister)
	srv.Handle("heartbeat", s.nativeHeartbeat)
	srv.Handle("report", s.nativeReport)
	srv.Handle("ping", s.nativePing)
	srv.Handle("roster", s.nativeRoster)
	srv.Handle("list_settings", s.nativeListSettings)
	srv.Handle("set_setting", s.nativeSetSetting)
	srv.Handle("reset_setting", s.nativeResetSetting)
	srv.Handle("report_setting", s.nativeReportSetting)
	return srv, nil
}

func objectSchema() map[string]any { return map[string]any{"type": "object"} }

func nativeError(call *spv2.Call, err error) error {
	return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
}

func (s *Server) nativeRegister(_ context.Context, call *spv2.Call) error {
	var reg protocol.RegisterPayload
	if err := call.Frame.DecodeBody(&reg); err != nil {
		return nativeError(call, err)
	}
	if reg.Name == "" || reg.Addr == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "register: name and addr required", false, nil)
	}
	nc := &nativeConn{call: call, name: reg.Name}
	roster, changed := s.hub.Register(reg.Card(), nc)
	declChanged := s.settings.Declare(reg.Name, reg.Settings)
	if err := call.Reply(roster); err != nil {
		return err
	}
	if changed {
		s.hub.Broadcast(roster)
		s.logger.Printf("原生注册: %s (%s)", reg.Name, reg.Addr)
	} else {
		s.logger.Printf("原生重连: %s（名片无变化）", reg.Name)
	}
	if len(reg.Settings) > 0 {
		if items := s.settings.PushItems(reg.Name); len(items) > 0 {
			go s.pushSettings(reg.Name, items, "register")
		} else if declChanged {
			s.broadcastSettings(protocol.SettingsEventPayload{Cause: "register", Service: reg.Name})
		}
	} else if declChanged {
		s.broadcastSettings(protocol.SettingsEventPayload{Cause: "register", Service: reg.Name})
	}
	return nil
}

func (s *Server) nativeHeartbeat(_ context.Context, call *spv2.Call) error {
	var hb protocol.HeartbeatPayload
	_ = call.Frame.DecodeBody(&hb)
	from := call.Frame.From.ID
	if hb.Status == "bye" {
		if roster, changed := s.hub.MarkOffline(from); changed {
			s.hub.Broadcast(roster)
			s.logger.Printf("原生下线: %s（告别）", from)
		}
		return nil
	}
	conn, _ := s.hub.Conn(from)
	if roster, resurrected := s.hub.NoteSeen(from, conn); resurrected {
		s.hub.Broadcast(roster)
	}
	return nil
}

func (s *Server) nativeReport(_ context.Context, call *spv2.Call) error {
	var p protocol.ReportPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return nativeError(call, err)
	}
	if p.Message == "" {
		return call.ReplyError(spv2.CodeInvalidArgument, "report: message 必填", false, nil)
	}
	from := call.Frame.From.ID
	kind := p.Kind
	if kind == "" {
		kind = "-"
	}
	s.logger.Printf("原生上报 %s [%s]: %s", from, kind, p.Message)
	if fn := s.cfg.OnReport; fn != nil {
		fn(from, p)
	}
	return call.Reply(map[string]any{"status": "ok"})
}

func (s *Server) nativePing(_ context.Context, call *spv2.Call) error {
	return call.Reply(map[string]any{"pong": true, "who": selfServiceName})
}

func (s *Server) nativeRoster(_ context.Context, call *spv2.Call) error {
	return call.Reply(s.hub.Snapshot())
}

func (s *Server) nativeListSettings(_ context.Context, call *spv2.Call) error {
	var p protocol.SettingsListPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return nativeError(call, err)
	}
	return call.Reply(protocol.SettingsResult{Services: s.settings.List(p.Service)})
}

func (s *Server) nativeSetSetting(_ context.Context, call *spv2.Call) error {
	var p protocol.SetSettingPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return nativeError(call, err)
	}
	ss, err := s.settings.Set(p.Service, p.Key, p.Value)
	if err != nil {
		return nativeError(call, err)
	}
	s.applyOrPush(p.Service, ss, p.Key, "set_setting", p.Value)
	return call.Reply(protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

func (s *Server) nativeResetSetting(_ context.Context, call *spv2.Call) error {
	var p protocol.ResetSettingPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return nativeError(call, err)
	}
	ss, err := s.settings.Reset(p.Service, p.Key)
	if err != nil {
		return nativeError(call, err)
	}
	s.applyOrPush(p.Service, ss, p.Key, "reset_setting", "")
	return call.Reply(protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}

func (s *Server) nativeReportSetting(_ context.Context, call *spv2.Call) error {
	var p protocol.ReportSettingPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return nativeError(call, err)
	}
	ss, err := s.settings.Report(p.Service, p.Key, p.Value, p.Set, p.Seq)
	if err != nil {
		return nativeError(call, err)
	}
	s.broadcastSettings(protocol.SettingsEventPayload{
		Cause: "report_setting", Service: p.Service,
		Results: []protocol.SettingResult{{Key: p.Key, OK: true}},
	})
	return call.Reply(protocol.SettingsResult{Services: []protocol.ServiceSettings{ss}})
}
