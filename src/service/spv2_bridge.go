package service

// SPv2.1 服务端与出站调用：服务卡由已注册的原生 method 生成，
// 入站/出站都只走 /spv2。

import (
	"context"
	"sort"
	"strings"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func genericSchema() map[string]any {
	return map[string]any{"type": "object"}
}

// buildSPv2 根据已注册的原生 method 与服务声明生成 SPv2 服务端。
func (s *Service) buildSPv2() error {
	// configure 是骨架保留的设置下发方法；原生 /spv2 直接解 body。
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
	nativeNames := make([]string, 0, len(s.native))
	for name := range s.native {
		nativeNames = append(nativeNames, name)
	}
	sort.Strings(nativeNames)
	methods := make([]spv2.Method, 0, len(nativeNames)+3)
	seen := map[string]bool{}
	for _, name := range nativeNames {
		m := s.native[name].Method
		if m.TimeoutMS == 0 {
			m.TimeoutMS = s.cfg.ActionTimeout.Milliseconds()
		}
		methods = append(methods, m)
		seen[name] = true
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
	maxBody := s.maxMessageBytes()
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
	// 骨架的日志必须接到 SPv2 服务端：丢弃帧与处理方 panic 要落在这个服务自己的日志里，
	// 而不是默认的 process stderr（见 src/spv2/server.go 的 countDropped / countPanic）。
	srv, err := spv2.NewServer(card, spv2.WithMaxMessageBytes(maxBody), spv2.WithLogger(s.logger))
	if err != nil {
		return err
	}
	for _, name := range nativeNames {
		srv.Handle(name, spv2.Handler(s.native[name].Handler))
	}
	s.spv2Srv = srv
	return nil
}

// callSPv2 向目标 /spv2 发一跳。
func (s *Service) callSPv2(ctx context.Context, name, addr, action string, raw msgpack.RawMessage, trace string) (msgpack.RawMessage, error) {
	target := spv2Addr(addr)
	c, err := spv2.Dial(ctx, target, spv2.WithClientMaxMessageBytes(s.maxMessageBytes()))
	if err != nil {
		return nil, err
	}
	defer c.Close()
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

func (s *Service) maxMessageBytes() int64 {
	if s.cfg.MaxMessageBytes > 0 {
		return s.cfg.MaxMessageBytes
	}
	return 1 << 20
}

func spv2Addr(addr string) string {
	if strings.HasSuffix(addr, "/spv2") {
		return addr
	}
	return strings.TrimSuffix(addr, "/") + "/spv2"
}
