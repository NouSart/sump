// settings-center 离线判定测试：心跳超时摘除（含广播）/ 告别立即摘除 / 重连去重 /
// 中心自身设置（扫描间隔与兜底心跳）即时生效。
package tests

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/settings-center/server"
)

// registerRaw 用原始客户端注册一个服务并保持连接（不跑心跳循环）。
// hbMS 为声明的心跳间隔（毫秒）；desc 为简介（用于“名片变化”用例）。
func registerRaw(t *testing.T, wsURL, name string, hbMS int64, desc string) *protocol.Client {
	t.Helper()
	c, err := protocol.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, name, "settings-center", "", protocol.RegisterPayload{
		Name: name, Addr: "ws://127.0.0.1:1/ws", Description: desc, HeartbeatMS: hbMS,
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("注册失败: %s", rp.Error)
	}
	return c
}

// pullRoster 通过 jump roster 拉取最新全量清单。
func pullRoster(t *testing.T, wsURL string) protocol.RosterPayload {
	t.Helper()
	c, err := protocol.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "settings-center", "", protocol.JumpPayload{Action: "roster"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("roster 拉取失败: %s", rp.Error)
	}
	var roster protocol.RosterPayload
	if err := protocol.DecodeRaw(rp.Data, &roster); err != nil {
		t.Fatal(err)
	}
	return roster
}

// hasService 判断名册是否包含某服务。
func hasService(roster protocol.RosterPayload, name string) bool {
	for _, s := range roster.Services {
		if s.Name == name {
			return true
		}
	}
	return false
}

// waitInRoster 轮询等待某服务在名册中出现 / 消失。
func waitInRoster(t *testing.T, wsURL, name string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if hasService(pullRoster(t, wsURL), name) == want {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("等待 %s 出现在名册=%v 超时", name, want)
}

// newFastCenter 起一个快节奏设置中心（扫描 40ms）。
func newFastCenter(t *testing.T) (*server.Server, string) {
	t.Helper()
	s, err := server.Start(server.Config{
		Addr:      "127.0.0.1:0",
		Heartbeat: 100 * time.Millisecond,
		Sweep:     40 * time.Millisecond,
	}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	return s, "ws://" + s.Addr() + "/ws"
}

// TestOfflineSweepAndBroadcast 验证：心跳超时（2× 声明间隔）→ 摘除名册 + 广播事件。
func TestOfflineSweepAndBroadcast(t *testing.T) {
	_, wsURL := newFastCenter(t)

	// 观察者：以服务身份注册（只有注册连接才在中心广播名单里），坐收 roster 事件
	var mu sync.Mutex
	var rosters [][]string
	obs, err := protocol.Dial(wsURL, func(env protocol.Envelope) {
		if env.Type != protocol.TypeRoster {
			return
		}
		var r protocol.RosterPayload
		if env.DecodePayload(&r) != nil {
			return
		}
		names := make([]string, 0, len(r.Services))
		for _, s := range r.Services {
			names = append(names, s.Name)
		}
		mu.Lock()
		rosters = append(rosters, names)
		mu.Unlock()
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })

	octx, ocancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer ocancel()
	regEnv, err := protocol.NewEnvelope(protocol.TypeRegister, "obs", "settings-center", "", protocol.RegisterPayload{
		Name: "obs", Addr: "ws://127.0.0.1:1/ws", Description: "观察者", HeartbeatMS: 60000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := obs.Call(octx, regEnv); err != nil {
		t.Fatal(err)
	}

	// 注册 svc-x：声明心跳 100ms，但之后不发心跳 → 约 200ms 后被判定离线
	registerRaw(t, wsURL, "svc-x", 100, "离线测试服务")
	waitInRoster(t, wsURL, "svc-x", true, 3*time.Second)
	waitInRoster(t, wsURL, "svc-x", false, 5*time.Second)

	// 广播序列应“先含 svc-x（注册）后不含（摘除）”
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		seenWith, removedAfter := false, false
		for _, names := range rosters {
			has := false
			for _, n := range names {
				if n == "svc-x" {
					has = true
				}
			}
			if has {
				seenWith = true
			}
			if seenWith && !has {
				removedAfter = true
			}
		}
		mu.Unlock()
		if removedAfter {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatal("未收到“先含 svc-x 后摘除”的 roster 广播序列")
}

// TestByeRemovesImmediately 验证：告别心跳 → 立即摘除（不等超时）。
func TestByeRemovesImmediately(t *testing.T) {
	_, wsURL := newFastCenter(t)

	// 声明心跳 60s：正常超时要等 2 分钟；bye 应立即生效
	c := registerRaw(t, wsURL, "svc-y", 60000, "告别测试服务")
	waitInRoster(t, wsURL, "svc-y", true, 3*time.Second)

	env, err := protocol.NewEnvelope(protocol.TypeHeartbeat, "svc-y", "settings-center", "", protocol.HeartbeatPayload{Status: "bye"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Send(env); err != nil {
		t.Fatal(err)
	}
	waitInRoster(t, wsURL, "svc-y", false, 2*time.Second)
}

// TestRegisterDedup 验证重连去重：名片无变化不推进 revision；名片变化才推进。
func TestRegisterDedup(t *testing.T) {
	_, wsURL := newFastCenter(t)

	rev := func() int64 { return pullRoster(t, wsURL).Revision }

	r0 := rev()
	c1 := registerRaw(t, wsURL, "svc-d", 60000, "去重测试")
	_ = c1
	r1 := rev()
	if r1 <= r0 {
		t.Fatalf("首次注册应推进 revision: %d -> %d", r0, r1)
	}

	// 同名同名片重连（模拟断线重连）：只换连接，不推进 revision
	c2 := registerRaw(t, wsURL, "svc-d", 60000, "去重测试")
	_ = c2
	if r2 := rev(); r2 != r1 {
		t.Fatalf("名片无变化不应推进 revision: %d -> %d", r1, r2)
	}

	// 名片变化（简介变化）：推进 revision
	c3 := registerRaw(t, wsURL, "svc-d", 60000, "去重测试（改了简介）")
	_ = c3
	if r3 := rev(); r3 <= r1 {
		t.Fatalf("名片变化应推进 revision: %d -> %d", r1, r3)
	}
}

// TestSweepSettingsLive 验证：中心自身设置**行为级**生效——
// 默认（扫描 5s / 兜底 15s）时，未声明心跳的服务要 ~30s 才判离线；
// 下发（扫描 100ms / 兜底 300ms）后，同一情形 ~1s 内即被摘除（扫描间隔变更立即重排）。
func TestSweepSettingsLive(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	for _, p := range []protocol.SetSettingPayload{
		{Service: "settings-center", Key: "sweep_ms", Value: "100"},
		{Service: "settings-center", Key: "heartbeat_fallback_ms", Value: "300"},
	} {
		if _, msg := callSettings(t, wsURL, "set_setting", p); msg != "" {
			t.Fatalf("%s 下发失败: %s", p.Key, msg)
		}
	}

	// 未声明心跳（hbMS=0）且不发心跳：兜底 300ms → 2×300ms 后应判离线。
	registerRaw(t, wsURL, "svc-f", 0, "兜底心跳测试")
	waitInRoster(t, wsURL, "svc-f", true, 3*time.Second)
	start := time.Now()
	waitInRoster(t, wsURL, "svc-f", false, 5*time.Second)
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("兜底=300ms 时应 ~1s 内判离线，实际 %v", elapsed)
	}
}
