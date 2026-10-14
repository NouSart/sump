// 骨架设置能力测试：configure 下发（校验/应用/拒绝）与注册后拉取兜底。
package tests

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/service"
)

// pushConfigure 以设置中心身份向服务地址推送一条 configure（单键）并取回执。
func pushConfigure(t *testing.T, addr string, p protocol.ConfigurePayload) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := protocol.Dial(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	env, err := protocol.NewEnvelope(protocol.TypeJump, "settings-center", "svc-s", "trace-cfg",
		protocol.JumpPayload{Action: "configure", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// newRecorder 返回线程安全的应用记录器。
func newRecorder() (func(v string) error, func() []string) {
	var mu sync.Mutex
	var got []string
	rec := func(v string) error {
		mu.Lock()
		got = append(got, v)
		mu.Unlock()
		return nil
	}
	snapshot := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), got...)
	}
	return rec, snapshot
}

// TestSettingConfigure 验证：声明进名片、configure 单键应用、非法值被拒且保持旧值、未声明键被拒。
func TestSettingConfigure(t *testing.T) {
	sc := startStubCenter(t)
	logger := log.New(os.Stdout, "[svc-test] ", log.LstdFlags)

	recMode, gotMode := newRecorder()
	s := service.New(service.Config{
		Name:              "svc-s",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Logger:            logger,
	})
	s.HandleSetting("demo.mode", "each", func(v string) error {
		if v != "each" && v != "batch" {
			return fmt.Errorf("不接受值 %q（可选 each / batch）", v)
		}
		return recMode(v)
	})
	s.HandleSetting("demo.size", "10", func(v string) error { return recMode(v) })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	regs := sc.Registers()
	if len(regs) == 0 {
		t.Fatal("未收到注册")
	}
	if got := regs[0].Settings; len(got) != 2 || got[0].Key != "demo.mode" || got[0].Default != "each" || got[1].Key != "demo.size" || got[1].Default != "10" {
		t.Fatalf("名片设置声明不符: %+v", got)
	}
	if v := s.SettingValue("demo.mode"); v != "each" {
		t.Fatalf("未下发时 SettingValue 应为预设 each，实际 %q", v)
	}

	// 正常下发：立即应用并回执。
	rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "demo.mode", Value: "batch", Default: "each", Overridden: true})
	if !rp.OK {
		t.Fatalf("configure 应成功: %+v", rp)
	}
	if v := s.SettingValue("demo.mode"); v != "batch" {
		t.Fatalf("生效值应为 batch，实际 %q", v)
	}
	if got := gotMode(); len(got) != 1 || got[0] != "batch" {
		t.Fatalf("应用回调记录不符: %+v", got)
	}

	// 非法值：拒绝、保持旧值。
	rp = pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "demo.mode", Value: "nope", Default: "each", Overridden: true})
	if rp.OK || !strings.Contains(rp.Error, "不接受值") || !strings.Contains(rp.Error, "demo.mode") {
		t.Fatalf("非法值应被拒绝并带上下文: %+v", rp)
	}
	if v := s.SettingValue("demo.mode"); v != "batch" {
		t.Fatalf("拒绝后应保持旧值 batch，实际 %q", v)
	}
	if got := gotMode(); len(got) != 1 {
		t.Fatalf("拒绝的值不应进入回调: %+v", got)
	}

	// 未声明键：拒绝。
	rp = pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "ghost", Value: "x"})
	if rp.OK || !strings.Contains(rp.Error, "未声明的设置项") {
		t.Fatalf("未声明键应被拒绝: %+v", rp)
	}
}

// TestSettingPullOnRegister 验证：注册成功后骨架自动拉取，只应用覆盖项；手动 RefreshSettings 幂等。
func TestSettingPullOnRegister(t *testing.T) {
	sc := startStubCenter(t)
	sc.SetSettings(protocol.SettingsResult{Services: []protocol.ServiceSettings{{
		Service: "svc-s",
		Settings: []protocol.SettingView{
			{Key: "demo.mode", Value: "batch", Default: "each", Overridden: true},
			{Key: "demo.plain", Value: "p", Default: "p"},
		},
	}}})
	logger := log.New(os.Stdout, "[svc-test] ", log.LstdFlags)

	recMode, gotMode := newRecorder()
	recPlain, gotPlain := newRecorder()
	s := service.New(service.Config{
		Name:              "svc-s",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Logger:            logger,
	})
	s.HandleSetting("demo.mode", "each", recMode)
	s.HandleSetting("demo.plain", "p", recPlain)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	// 自动拉取：覆盖项应生效。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := gotMode(); len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got := gotMode(); len(got) == 0 || got[0] != "batch" {
		t.Fatalf("注册后应自动拉取并应用覆盖项，实际 %+v", got)
	}
	if v := s.SettingValue("demo.mode"); v != "batch" {
		t.Fatalf("拉取后生效值应为 batch，实际 %q", v)
	}

	// 手动刷新：幂等（重复应用同一值，无副作用）。
	s.RefreshSettings(ctx)
	time.Sleep(100 * time.Millisecond)
	for _, v := range gotMode() {
		if v != "batch" {
			t.Fatalf("手动刷新只应重复应用覆盖值，实际 %+v", gotMode())
		}
	}

	// 非覆盖项不得被拉取应用。
	if got := gotPlain(); len(got) != 0 {
		t.Fatalf("非覆盖项不应被应用，实际 %+v", got)
	}
}

// logBuffer 是并发安全的日志缓冲（读循环与测试并发写）。
type logBuffer struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *logBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *logBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestSettingsEventIgnored 验证：settings 事件是**已知协议事件**——骨架静默忽略，
// 不记“未知事件”（该分支只留给真正未知的事件类型），且不影响后续名册更新。
func TestSettingsEventIgnored(t *testing.T) {
	sc := startStubCenter(t)
	lb := &logBuffer{}
	s := service.New(service.Config{
		Name:              "svc-e",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Logger:            log.New(lb, "", 0),
	})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	sc.Broadcast(protocol.TypeSettings, protocol.SettingsEventPayload{
		Cause: "set_setting", Service: "svc-e",
		Results: []protocol.SettingResult{{Key: "k", Value: "v", OK: true}},
	})
	// 真正的未知事件仍应记日志（对照组），确保静默只针对 settings。
	sc.Broadcast("mystery_event", map[string]any{"x": 1})
	// 事件之后名册照常工作。
	sc.Broadcast(protocol.TypeRoster, protocol.RosterPayload{
		Services: []protocol.ServiceCard{
			{Name: "settings-center", Addr: sc.URL()},
			{Name: "svc-x", Addr: "ws://127.0.0.1:1/ws"},
		},
		Revision: 2,
	})

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(s.Roster()) < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(s.Roster()) != 2 {
		t.Fatalf("settings 事件后名册应照常更新: %+v", s.Roster())
	}
	if strings.Contains(lb.String(), "未知事件: settings") {
		t.Fatalf("settings 是已知事件，不应记为未知: %s", lb.String())
	}
	if !strings.Contains(lb.String(), "未知事件: mystery_event") {
		t.Fatalf("真正未知的事件仍应记日志: %s", lb.String())
	}
}
