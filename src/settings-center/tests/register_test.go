// Package tests 是 settings-center 的服务内集成测试：
// 起真实 SPv2 服务，用原生客户端走真实连接。
package tests

import (
	"log"
	"os"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
	"github.com/NouSart/sump/spv2"
)

func newLogger() *log.Logger {
	return log.New(os.Stdout, "[sc-test] ", log.LstdFlags)
}

// registerPayload 发一条完整 register 载荷，返回中心回执里的名册。
func registerPayload(t *testing.T, c *spv2.Client, p protocol.RegisterPayload) protocol.RosterPayload {
	t.Helper()
	f, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(p.Name), spv2.ServiceRef("settings-center"), "register", p)
	if err != nil {
		t.Fatal(err)
	}
	var roster protocol.RosterPayload
	if err := c.CallDecode(t.Context(), f, &roster); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	return roster
}

// TestRegisterAndRosterBroadcast 验证：注册成功、响应带 roster、
// 新服务加入后 roster 事件广播到已连接服务。
func TestRegisterAndRosterBroadcast(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	url := centerURL(s)

	events1 := make(chan protocol.RosterPayload, 16)
	c1 := dialCenter(t, url, spv2.WithOnEvent(func(f spv2.Frame) {
		if f.Type != spv2.TypeEvent || f.Method != protocol.TypeRoster {
			return
		}
		var r protocol.RosterPayload
		if f.DecodeBody(&r) == nil {
			events1 <- r
		}
	}))

	// 服务 A 注册
	rosterA := registerPayload(t, c1, protocol.RegisterPayload{
		Name: "svc-a", Addr: "ws://127.0.0.1:1/spv2", Description: "测试服务 A",
		Provides: []protocol.Provide{{Action: "ping", Output: "pong"}},
	})
	if len(rosterA.Services) != 2 || rosterA.Services[0].Name != "settings-center" || rosterA.Services[1].Name != "svc-a" {
		t.Fatalf("A 的注册响应 roster 不符: %+v", rosterA)
	}

	// 服务 B 注册（新连接）
	c2 := dialCenter(t, url)
	rosterB := registerPayload(t, c2, protocol.RegisterPayload{
		Name: "svc-b", Addr: "ws://127.0.0.1:2/spv2", Description: "测试服务 B",
	})
	if len(rosterB.Services) != 3 || rosterB.Services[1].Name != "svc-a" || rosterB.Services[2].Name != "svc-b" {
		t.Fatalf("B 的注册响应 roster 不符: %+v", rosterB)
	}

	// A 应通过事件收到含三个服务（含设置中心自身）的 roster 广播
	deadline := time.After(5 * time.Second)
	for {
		select {
		case r := <-events1:
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
	url := centerURL(s)

	c1 := dialCenter(t, url)
	registerPayload(t, c1, protocol.RegisterPayload{Name: "svc-q", Addr: "ws://127.0.0.1:3/spv2"})

	var roster protocol.RosterPayload
	if err := callCenter(t, c1, "roster", nil, &roster); err != nil {
		t.Fatalf("roster 拉取失败: %v", err)
	}
	if len(roster.Services) != 2 {
		t.Fatalf("清单应含 settings-center 与 svc-q，实际: %+v", roster)
	}
	if roster.Services[0].Name != "settings-center" || roster.Services[1].Name != "svc-q" {
		t.Fatalf("清单内容不符: %+v", roster)
	}
}
