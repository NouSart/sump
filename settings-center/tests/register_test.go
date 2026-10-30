// Package tests 是 settings-center 的服务内集成测试：
// 起真实 WS 服务，用协议客户端走真实连接。
package tests

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
)

func newLogger() *log.Logger {
	return log.New(os.Stdout, "[sc-test] ", log.LstdFlags)
}

// TestRegisterAndRosterBroadcast 验证：注册成功、响应带 roster、
// 新服务加入后 roster 事件广播到已连接服务。
func TestRegisterAndRosterBroadcast(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	events1 := make(chan protocol.Envelope, 16)
	c1, err := protocol.Dial(wsURL, func(env protocol.Envelope) { events1 <- env })
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c1.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// 服务 A 注册
	regA, err := protocol.NewEnvelope(protocol.TypeRegister, "svc-a", "settings-center", "", protocol.RegisterPayload{
		Name: "svc-a", Addr: "ws://127.0.0.1:1/ws", Description: "测试服务 A",
		Provides: []protocol.Provide{{Action: "ping", Output: "pong"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	respA, err := c1.Call(ctx, regA)
	if err != nil {
		t.Fatal(err)
	}
	var rpA protocol.ResponsePayload
	if err := respA.DecodePayload(&rpA); err != nil {
		t.Fatal(err)
	}
	if !rpA.OK {
		t.Fatalf("A 注册失败: %s", rpA.Error)
	}
	var rosterA protocol.RosterPayload
	if err := protocol.DecodeRaw(rpA.Data, &rosterA); err != nil {
		t.Fatal(err)
	}
	if len(rosterA.Services) != 2 || rosterA.Services[0].Name != "settings-center" || rosterA.Services[1].Name != "svc-a" {
		t.Fatalf("A 的注册响应 roster 不符: %+v", rosterA)
	}

	// 服务 B 注册（新连接）
	c2, err := protocol.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c2.Close() })

	regB, err := protocol.NewEnvelope(protocol.TypeRegister, "svc-b", "settings-center", "", protocol.RegisterPayload{
		Name: "svc-b", Addr: "ws://127.0.0.1:2/ws", Description: "测试服务 B",
	})
	if err != nil {
		t.Fatal(err)
	}
	respB, err := c2.Call(ctx, regB)
	if err != nil {
		t.Fatal(err)
	}
	var rpB protocol.ResponsePayload
	if err := respB.DecodePayload(&rpB); err != nil {
		t.Fatal(err)
	}
	if !rpB.OK {
		t.Fatalf("B 注册失败: %s", rpB.Error)
	}
	var rosterB protocol.RosterPayload
	if err := protocol.DecodeRaw(rpB.Data, &rosterB); err != nil {
		t.Fatal(err)
	}
	if len(rosterB.Services) != 3 || rosterB.Services[1].Name != "svc-a" || rosterB.Services[2].Name != "svc-b" {
		t.Fatalf("B 的注册响应 roster 不符: %+v", rosterB)
	}

	// A 应通过事件收到含三个服务（含设置中心自身）的 roster 广播
	deadline := time.After(5 * time.Second)
	for {
		select {
		case ev := <-events1:
			if ev.Type != protocol.TypeRoster {
				continue
			}
			var r protocol.RosterPayload
			if err := ev.DecodePayload(&r); err != nil {
				continue
			}
			if len(r.Services) == 3 {
				if r.Services[0].Name != "settings-center" || r.Services[1].Name != "svc-a" || r.Services[2].Name != "svc-b" {
					t.Fatalf("roster 排序 / 内容不符: %+v", r)
				}
				if r.Revision < 3 {
					t.Fatalf("revision 应 ≥ 3: %d", r.Revision)
				}
				return // 成功
			}
		case <-deadline:
			t.Fatal("未收到含三个服务的 roster 广播")
		}
	}
}

// TestRosterQuery 验证 DNS 式拉取：服务主动访问设置中心获取最新全量清单。
func TestRosterQuery(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	c1, err := protocol.Dial(wsURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c1.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	reg, err := protocol.NewEnvelope(protocol.TypeRegister, "svc-q", "settings-center", "", protocol.RegisterPayload{
		Name: "svc-q", Addr: "ws://127.0.0.1:3/ws",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c1.Call(ctx, reg); err != nil {
		t.Fatal(err)
	}

	query, err := protocol.NewEnvelope(protocol.TypeJump, "svc-q", "settings-center", "", protocol.JumpPayload{Action: "roster"})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c1.Call(ctx, query)
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
	if len(roster.Services) != 2 {
		t.Fatalf("清单应含 settings-center 与 svc-q，实际: %+v", roster)
	}
	if roster.Services[0].Name != "settings-center" || roster.Services[1].Name != "svc-q" {
		t.Fatalf("清单内容不符: %+v", roster)
	}
}
