// Package tests 是 reasoner 的服务内集成测试：
// 最小“设置中心”替身 + 真实 reasoner 实例，走真实 WS 通信。
package tests

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/server"
)

// ---------- 设置中心的最小替身 ----------

type stubConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *stubConn) send(env protocol.Envelope) {
	data, err := protocol.Marshal(env)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.BinaryMessage, data)
}

type stubCenter struct {
	ln     net.Listener
	srv    *http.Server
	logger *log.Logger

	mu       sync.Mutex
	cards    map[string]protocol.ServiceCard
	conns    map[*stubConn]struct{}
	settings *protocol.SettingsResult   // 非 nil 时响应 list_settings
	regs     []protocol.RegisterPayload // 收到的注册（按到达顺序；重注册会再次出现）
}

var stubUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{
		ln:     ln,
		logger: log.New(os.Stdout, "[stub-center] ", log.LstdFlags),
		cards:  map[string]protocol.ServiceCard{},
		conns:  map[*stubConn]struct{}{},
	}
	sc.cards["stub-center"] = protocol.ServiceCard{
		Name:        "stub-center",
		Addr:        sc.URL(),
		Description: "设置中心测试替身",
		Provides: []protocol.Provide{
			{Action: "ping", Tool: true, Output: "pong"},
			{Action: "hidden_no_tool", Output: "未声明 tool,不应暴露"},
		},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sc.handleWS)
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/ws" }

func (sc *stubCenter) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			sc.mu.Lock()
			delete(sc.conns, c)
			sc.mu.Unlock()
			_ = ws.Close()
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeRegister:
			var reg protocol.RegisterPayload
			if err := env.DecodePayload(&reg); err != nil {
				continue
			}
			sc.mu.Lock()
			sc.cards[reg.Name] = reg.Card()
			sc.regs = append(sc.regs, reg)
			sc.conns[c] = struct{}{}
			roster := sc.snapshotLocked()
			sc.mu.Unlock()

			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true, Data: respData})
			c.send(resp)
			sc.broadcast(roster)
		case protocol.TypeJump:
			var jp protocol.JumpPayload
			if err := env.DecodePayload(&jp); err != nil {
				continue
			}
			switch jp.Action {
			case "ping":
				data, _ := protocol.EncodePayload(map[string]any{"pong": true, "who": "stub-center"})
				resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true, Data: data})
				c.send(resp)
			case "list_settings":
				sc.mu.Lock()
				settings := sc.settings
				sc.mu.Unlock()
				if settings == nil {
					resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
					c.send(resp)
					continue
				}
				data, _ := protocol.EncodePayload(*settings)
				resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true, Data: data})
				c.send(resp)
			}
		case protocol.TypeHeartbeat:
			// 忽略
		}
	}
}

func (sc *stubCenter) snapshotLocked() protocol.RosterPayload {
	names := make([]string, 0, len(sc.cards))
	for n := range sc.cards {
		names = append(names, n)
	}
	sort.Strings(names)
	services := make([]protocol.ServiceCard, 0, len(names))
	for _, n := range names {
		services = append(services, sc.cards[n])
	}
	return protocol.RosterPayload{Services: services, Revision: 1}
}

func (sc *stubCenter) broadcast(roster protocol.RosterPayload) {
	sc.mu.Lock()
	targets := make([]*stubConn, 0, len(sc.conns))
	for c := range sc.conns {
		targets = append(targets, c)
	}
	sc.mu.Unlock()
	for _, c := range targets {
		env, err := protocol.NewEnvelope(protocol.TypeRoster, "stub-center", "", "", roster)
		if err == nil {
			c.send(env)
		}
	}
}

// Registers 返回收到的注册 payload 快照（按到达顺序；重注册会再次出现）。
func (sc *stubCenter) Registers() []protocol.RegisterPayload {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	out := make([]protocol.RegisterPayload, len(sc.regs))
	copy(out, sc.regs)
	return out
}

// withSettings 让替身响应 list_settings：注册“settings-center”名片并给出设置结果
// （在 reasoner 启动前调用——它启动时会拉取一次）。
func (sc *stubCenter) withSettings(t *testing.T, res protocol.SettingsResult) {
	t.Helper()
	sc.mu.Lock()
	defer sc.mu.Unlock()
	sc.cards["settings-center"] = protocol.ServiceCard{
		Name: "settings-center", Addr: sc.URL(), Description: "设置中心替身",
		Provides: []protocol.Provide{{Action: "list_settings"}, {Action: "set_setting"}, {Action: "reset_setting"}},
	}
	sc.settings = &res
}

// ---------- 测试辅助 ----------

func startReasoner(t *testing.T, centerURL string) *server.Server {
	t.Helper()
	logger := log.New(os.Stdout, "[reasoner-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            centerURL,
		HeartbeatInterval: 100 * time.Millisecond,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

func dialReasoner(t *testing.T, s *server.Server) *protocol.Client {
	t.Helper()
	c, err := protocol.Dial(s.WsURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// jump 对 reasoner 发起一次跳转并返回响应 payload。
func jump(t *testing.T, c *protocol.Client, action string, input any, ctx context.Context) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: action, Input: raw})
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
	return rp
}

// ---------- 用例 ----------

// TestRegisterAndEcho 验证：注册后名册到账；echo 动作可用。
func TestRegisterAndEcho(t *testing.T) {
	sc := startStubCenter(t)
	s := startReasoner(t, sc.URL())

	roster := s.Roster()
	if len(roster) != 2 || roster[0].Name != "reasoner" || roster[1].Name != "stub-center" {
		t.Fatalf("名册不符: %+v", roster)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialReasoner(t, s)

	rp := jump(t, c, "echo", map[string]any{"msg": "hi"}, ctx)
	if !rp.OK {
		t.Fatalf("echo 失败: %s", rp.Error)
	}
	var out struct {
		Who  string `msgpack:"who"`
		Echo struct {
			Msg string `msgpack:"msg"`
		} `msgpack:"echo"`
	}
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Who != "reasoner" || out.Echo.Msg != "hi" {
		t.Fatalf("echo 结果不符: %+v", out)
	}
}

// TestSelfJump 验证自我跳转：debug_jump 到 self，再回 echo——
// 这是“循环 = 自我跳转链”的最小形态。
func TestSelfJump(t *testing.T) {
	sc := startStubCenter(t)
	s := startReasoner(t, sc.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialReasoner(t, s)

	rp := jump(t, c, "debug_jump", map[string]any{
		"to":     "self",
		"action": "echo",
		"input":  map[string]any{"n": 42},
	}, ctx)
	if !rp.OK {
		t.Fatalf("自跳失败: %s", rp.Error)
	}
	var out struct {
		Who  string `msgpack:"who"`
		Echo struct {
			N int `msgpack:"n"`
		} `msgpack:"echo"`
	}
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Who != "reasoner" || out.Echo.N != 42 {
		t.Fatalf("自跳结果不符: %+v", out)
	}
}

// TestJumpToCenter 验证跨服务跳转：debug_jump 经名册解析地址，访问 stub-center 的 ping。
func TestJumpToCenter(t *testing.T) {
	sc := startStubCenter(t)
	s := startReasoner(t, sc.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c := dialReasoner(t, s)

	rp := jump(t, c, "debug_jump", map[string]any{
		"to":     "stub-center",
		"action": "ping",
	}, ctx)
	if !rp.OK {
		t.Fatalf("跳转中心失败: %s", rp.Error)
	}
	var out struct {
		Pong bool   `msgpack:"pong"`
		Who  string `msgpack:"who"`
	}
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if !out.Pong || out.Who != "stub-center" {
		t.Fatalf("ping 结果不符: %+v", out)
	}
}
