// Package tests 是 reasoner 的服务内集成测试：
// 最小“设置中心”替身 + 真实 reasoner 实例，走真实 SPv2.1（/spv2）通信。
package tests

import (
	"context"
	"log"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/server"
	"github.com/NouSart/sump/spv2"
)

// ---------- 设置中心的最小替身（原生 /spv2：register 是 call，roster 是 event） ----------

type stubCenter struct {
	srv    *spv2.Server
	http   *httptest.Server
	tools  *stubEndpoint // “stub-center”工具端点：ping 回 pong
	logger *log.Logger

	mu       sync.Mutex
	cards    map[string]protocol.ServiceCard
	calls    map[string]*spv2.Call      // 服务名 → 中心连接（roster 事件往这上面推）
	settings *protocol.SettingsResult   // 非 nil 时响应 list_settings
	regs     []protocol.RegisterPayload // 收到的注册（按到达顺序；重注册会再次出现）
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "settings-center", Actor: spv2.ServiceRef("settings-center"),
		Version: "stub/1",
		Methods: []spv2.Method{
			{Name: "register", Shape: spv2.ShapeCall},
			{Name: "list_settings", Shape: spv2.ShapeCall},
			{Name: "report", Shape: spv2.ShapeCall},
			{Name: "report_setting", Shape: spv2.ShapeCall},
			{Name: "heartbeat", Shape: spv2.ShapeEvent},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{
		srv:    srv,
		logger: log.New(os.Stdout, "[stub-center] ", log.LstdFlags),
		cards:  map[string]protocol.ServiceCard{},
		calls:  map[string]*spv2.Call{},
	}
	srv.Handle("register", sc.handleRegister)
	srv.Handle("list_settings", sc.handleListSettings)
	srv.Handle("report", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(map[string]any{"status": "ok"})
	})
	srv.Handle("report_setting", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(map[string]any{"status": "ok"})
	})
	srv.Handle("heartbeat", func(context.Context, *spv2.Call) error { return nil })
	sc.http = httptest.NewServer(srv.Handler())
	t.Cleanup(sc.http.Close)

	// 工具端点单起一个 /spv2 服务（Actor 必须是 stub-center——SPv2 校验 to == 服务自己的 actor）。
	sc.tools = newStubEndpoint(t, "stub-center", spv2.Method{Name: "ping", Shape: spv2.ShapeCall})
	sc.tools.handle("ping", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(map[string]any{"pong": true, "who": "stub-center"})
	})
	registerStub(t, sc, protocol.RegisterPayload{
		Name: "stub-center", Addr: sc.tools.URL(), Description: "设置中心测试替身",
		Provides: []protocol.Provide{
			{Action: "ping", Tool: true, Output: "pong"},
			{Action: "hidden_no_tool", Output: "未声明 tool,不应暴露"},
		},
	})
	return sc
}

func (sc *stubCenter) URL() string {
	return "ws" + strings.TrimPrefix(sc.http.URL, "http") + "/spv2"
}

func (sc *stubCenter) handleRegister(_ context.Context, call *spv2.Call) error {
	var reg protocol.RegisterPayload
	if err := call.Frame.DecodeBody(&reg); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	sc.mu.Lock()
	sc.cards[reg.Name] = reg.Card()
	sc.regs = append(sc.regs, reg)
	sc.calls[reg.Name] = call
	roster := sc.snapshotLocked()
	sc.mu.Unlock()
	if err := call.Reply(roster); err != nil {
		return err
	}
	sc.broadcast(roster)
	return nil
}

func (sc *stubCenter) handleListSettings(_ context.Context, call *spv2.Call) error {
	sc.mu.Lock()
	settings := sc.settings
	sc.mu.Unlock()
	if settings == nil {
		return call.ReplyError(spv2.CodeNotFound, "unknown action: list_settings", false, nil)
	}
	return call.Reply(*settings)
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
	targets := make([]*spv2.Call, 0, len(sc.calls))
	for _, call := range sc.calls {
		targets = append(targets, call)
	}
	sc.mu.Unlock()
	for _, call := range targets {
		_ = call.SendEvent("roster", roster)
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

// ---------- 通用替身件 ----------

// stubEndpoint 是一个最小 SPv2.1 服务端：测试里“别的服务”都长这样（Actor = 自己的名字）。
type stubEndpoint struct {
	srv  *spv2.Server
	http *httptest.Server
	url  string
}

func newStubEndpoint(t *testing.T, name string, methods ...spv2.Method) *stubEndpoint {
	t.Helper()
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: name, Actor: spv2.ServiceRef(name),
		Version: "stub/1", Methods: methods, Limits: spv2.Limits{MaxBody: 4 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	e := &stubEndpoint{srv: srv, http: httptest.NewServer(srv.Handler())}
	e.url = "ws" + strings.TrimPrefix(e.http.URL, "http") + "/spv2"
	t.Cleanup(e.http.Close)
	return e
}

// URL 返回替身自己的 /spv2 地址（名册里的 addr 就填它）。
func (e *stubEndpoint) URL() string { return e.url }

// handle 注册一个原生 method。
func (e *stubEndpoint) handle(name string, h spv2.Handler) { e.srv.Handle(name, h) }

// registerStub 用一次原生 register call 把替身注册进桩中心（注册后连接即可关闭）。
func registerStub(t *testing.T, sc *stubCenter, p protocol.RegisterPayload) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, sc.URL())
	if err != nil {
		t.Fatalf("%s 注册失败: %v", p.Name, err)
	}
	defer c.Close()
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(p.Name), spv2.ServiceRef("settings-center"), "register", p)
	if err != nil {
		t.Fatal(err)
	}
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, call, &roster); err != nil {
		t.Fatalf("%s 注册失败: %v", p.Name, err)
	}
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

func dialReasoner(t *testing.T, s *server.Server) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), s.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// outcome 把原生 reply 摊平成测试关心的两件事：成功数据 / 错误文本。
type outcome struct {
	OK    bool
	Data  []byte
	Error string
}

// jump 以 from=test 对 reasoner 发起一次原生 call（work 固定在测试里不关心）。
func jump(t *testing.T, c *spv2.Client, action string, input any, ctx context.Context) outcome {
	t.Helper()
	return callFrom(t, c, "test", "w-test", action, input, ctx)
}

// callFrom 以 from 的身份向 reasoner 发起一次原生 call。
func callFrom(t *testing.T, c *spv2.Client, from, work, action string, input any, ctx context.Context) outcome {
	t.Helper()
	call, err := spv2.NewCall(work, spv2.ServiceRef(from), spv2.ServiceRef("reasoner"), action, input)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := c.Call(ctx, call)
	if err != nil {
		return outcome{Error: err.Error()}
	}
	return outcome{OK: true, Data: resp.Body}
}

// sendFrom 以入口服务身份发一条 message 并断言受理（spec §7.1 唯一入口）。
// SPv2 没有 boss 字段：from 既是入口，也是结果的归属方。
func sendFrom(t *testing.T, s *server.Server, from string, payload any) {
	t.Helper()
	var mp protocol.MessagePayload
	switch v := payload.(type) {
	case protocol.MessagePayload:
		mp = v
	case map[string]any:
		if text, ok := v["text"].(string); ok {
			mp.Text = text
		}
		if cid, ok := v["conversation_id"].(string); ok {
			mp.Conversation = cid
		}
		if sender, ok := v["sender"].(*protocol.Sender); ok {
			mp.Sender = sender
		}
		for _, key := range []string{"images", "files", "notes"} {
			var out []string
			switch val := v[key].(type) {
			case []string:
				out = val
			case []any:
				for _, item := range val {
					if str, ok := item.(string); ok {
						out = append(out, str)
					}
				}
			}
			switch key {
			case "images":
				mp.Images = out
			case "files":
				mp.Files = out
			case "notes":
				mp.Notes = out
			}
		}
	default:
		t.Fatalf("sendFrom 不支持的 payload 类型 %T", payload)
	}
	if mp.Sender == nil {
		mp.Sender = &protocol.Sender{Channel: "private", Name: "boss", Owner: true}
	}
	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(from), spv2.ServiceRef("reasoner"), "message", mp)
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Status string `msgpack:"status"`
	}
	if err := c.CallDecode(ctx, call, &ack); err != nil {
		t.Fatalf("message 未被受理: %v", err)
	}
	if ack.Status != "accepted" {
		t.Fatalf("message 未被受理: %+v", ack)
	}
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
