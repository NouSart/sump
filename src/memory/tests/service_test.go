package tests

// memory 服务级测试：真 WS + 真 SQLite。
// 覆盖：store 落库 → recall 托管上下文 → 组装记忆 → resume 发回（from=memory）回路；
// 去重（任务链已有消息不重复注入）与记忆块替换。

import (
	"context"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/experience"
	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/memory/facade"
	"github.com/NouSart/sump/memory/facade/compose"
	"github.com/NouSart/sump/memory/session"
	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// ---------- 设置中心替身 ----------

type stubCenter struct {
	srv  *spv2.Server
	http *httptest.Server

	mu       sync.Mutex
	cards    map[string]protocol.ServiceCard
	settings map[string][]protocol.Setting
	calls    map[string]*spv2.Call
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	card := spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "settings-center", Actor: spv2.ServiceRef("settings-center"),
		Version: "stub/1", Methods: []spv2.Method{
			{Name: "register", Shape: spv2.ShapeCall},
			{Name: "heartbeat", Shape: spv2.ShapeEvent},
			{Name: "list_settings", Shape: spv2.ShapeCall},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	}
	srv, err := spv2.NewServer(card)
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{
		srv: srv, cards: map[string]protocol.ServiceCard{},
		settings: map[string][]protocol.Setting{}, calls: map[string]*spv2.Call{},
	}
	srv.Handle("register", sc.handleRegister)
	srv.Handle("heartbeat", func(_ context.Context, _ *spv2.Call) error { return nil })
	srv.Handle("list_settings", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(protocol.SettingsResult{})
	})
	sc.http = httptest.NewServer(srv.Handler())
	t.Cleanup(sc.http.Close)
	sc.cards["stub-center"] = protocol.ServiceCard{
		Name: "stub-center", Addr: sc.URL(), Description: "设置中心测试替身",
		Provides: []protocol.Provide{{Action: "list_settings"}, {Action: "roster"}},
	}
	return sc
}

func (sc *stubCenter) URL() string {
	return "ws" + strings.TrimPrefix(sc.http.URL, "http") + "/spv2"
}

// DeclaredSettings 返回某服务最近一次注册声明的设置项。
func (sc *stubCenter) DeclaredSettings(name string) []protocol.Setting {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.Setting(nil), sc.settings[name]...)
}

func (sc *stubCenter) handleRegister(_ context.Context, call *spv2.Call) error {
	var reg protocol.RegisterPayload
	if err := call.Frame.DecodeBody(&reg); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	sc.mu.Lock()
	sc.cards[reg.Name] = reg.Card()
	sc.settings[reg.Name] = reg.Settings
	sc.calls[reg.Name] = call
	roster := sc.snapshotLocked()
	sc.mu.Unlock()
	if err := call.Reply(roster); err != nil {
		return err
	}
	sc.broadcast(roster)
	return nil
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

// registerService 用一个原生 call 把替身注册进名册（注册后连接即可关闭）。
func registerService(t *testing.T, centerURL, name, addr string) {
	t.Helper()
	c, err := spv2.Dial(context.Background(), centerURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call, err := spv2.NewCall("w-reg-"+name, spv2.ServiceRef(name), spv2.ServiceRef("settings-center"), "register",
		protocol.RegisterPayload{Name: name, Addr: addr, Description: name + " 替身"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, call, &roster); err != nil {
		t.Fatalf("%s 注册失败: %v", name, err)
	}
}

// ---------- 调用方替身（扮演 reasoner） ----------

type stubCaller struct {
	srv  *spv2.Server
	http *httptest.Server
}

func startStubCaller(t *testing.T, centerURL string) *stubCaller {
	t.Helper()
	c := &stubCaller{}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "reasoner", Actor: spv2.ServiceRef("reasoner"),
		Version: "stub/1", Methods: []spv2.Method{},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.srv = srv
	c.http = httptest.NewServer(srv.Handler())
	t.Cleanup(c.http.Close)
	registerService(t, centerURL, "reasoner", c.URL())
	return c
}

func (c *stubCaller) URL() string {
	return "ws" + strings.TrimPrefix(c.http.URL, "http") + "/spv2"
}

// call 向 memory 发一次原生调用并等回执（短连接）。默认帧头 work 固定为 trace-test；
// 需要「一个 work 换任务」的用例走 callAsWork（work 是执行单元，换任务会显式冲突）。
func (c *stubCaller) call(t *testing.T, memoryURL, action string, input any) callResult {
	t.Helper()
	return c.callAsWork(t, memoryURL, "trace-test", action, input)
}

// callAsWork 同 call，但指定帧头 work。
func (c *stubCaller) callAsWork(t *testing.T, memoryURL, work, action string, input any) callResult {
	t.Helper()
	conn, err := spv2.Dial(context.Background(), memoryURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	call, err := spv2.NewCall(work, spv2.ServiceRef("reasoner"), spv2.ServiceRef("memory"), action, input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := conn.Call(ctx, call)
	if err != nil {
		return callResult{OK: false, Error: err.Error()}
	}
	return callResult{OK: true, Data: resp.Body}
}

// contextMessages 是 context/2 的回执（session 基座 + 门面注入的经验块）。
type contextMessages = protocol.ContextResponse

// openWork 登记 work / 任务（生产链路的 work_open）。
func (c *stubCaller) openWork(t *testing.T, memoryURL, work, cid, taskName string) protocol.WorkOpenResult {
	t.Helper()
	rp := c.callAsWork(t, memoryURL, work, "work_open", protocol.WorkOpenPayload{
		Work: work, ConversationID: cid, TaskName: taskName, Boss: "stub-boss",
	})
	if !rp.OK {
		t.Fatalf("work_open 失败: %s", rp.Error)
	}
	var out protocol.WorkOpenResult
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatalf("work_open 回执无法解码: %v", err)
	}
	return out
}

// storeV2 同步落一条记录并返回回执（context 的 trigger.item 来自这里）。
func (c *stubCaller) storeV2(t *testing.T, memoryURL, work, cid, taskID, role, content, key string) protocol.StoreResult {
	t.Helper()
	rp := c.callAsWork(t, memoryURL, work, "store", protocol.StorePayload{
		ConversationID: cid, Work: work, TaskID: taskID, Boss: "stub-boss",
		Role: role, Content: content, ClientItemID: key,
	})
	if !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
	var out protocol.StoreResult
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatalf("store 回执无法解码: %v", err)
	}
	return out
}

// contextFor 发一次 context/2（指定帧头 work 与触发源）。
func (c *stubCaller) contextFor(t *testing.T, memoryURL, work string, trigger protocol.Trigger) protocol.ContextResponse {
	t.Helper()
	rp := c.callAsWork(t, memoryURL, work, "context", protocol.ContextRequest{Work: work, Trigger: trigger})
	if !rp.OK {
		t.Fatalf("context 失败: %s", rp.Error)
	}
	var out protocol.ContextResponse
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatalf("context 回执无法解码: %v", err)
	}
	return out
}

// decodeContext 解 context 回执；失败即测试失败。
func decodeContext(t *testing.T, rp callResult) contextMessages {
	t.Helper()
	var out contextMessages
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatalf("context 回执无法解码: %v", err)
	}
	return out
}

// stackConfig 把三个进程的启动配置交给用例调整；Store/Shared 是底层句柄。
type stackConfig struct {
	Session    session.Config
	Experience experience.Config
	Store      *store.Store
	Shared     *shared.Store
}

// testStack 是测试里的三进程栈。
type testStack struct {
	*facade.Server
	sess *session.Server
	exp  *experience.Server
	st   *store.Store // 会话库：端到端用例直接登记 work / 核对落库
}

// addPeer 把测试里后上线的 embedding/reasoner 地址注入 experience。
func (s *testStack) addPeer(name, addr string) { s.exp.SetPeer(name, addr) }

// metricsOf 读门面的 SPv2 metrics：这里必须看到内部服务聚合后的指标。
func metricsOf(t *testing.T, srv *testStack) []spv2.Metric {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, err := spv2.Dial(ctx, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	frame, err := spv2.NewCall("metrics-read", spv2.ServiceRef("test"), spv2.ServiceRef("memory"), "metrics", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := conn.Call(ctx, frame)
	if err != nil {
		t.Fatal(err)
	}
	var out spv2.MetricsResponse
	if err := protocol.DecodeRaw(resp.Body, &out); err != nil {
		t.Fatalf("解析 metrics 失败: %v", err)
	}
	return out.Metrics
}

// startMemory 起 session + experience + facade 三进程（临时库）。
func startMemory(t *testing.T, centerURL string, historyLimit int, opts ...func(*stackConfig)) *testStack {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	sh, err := shared.Open(filepath.Join(t.TempDir(), "experience.db"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })
	cfg := stackConfig{
		Session: session.Config{
			Name: "session", Listen: "127.0.0.1:0", Standalone: true,
			Store: st, HistoryLimit: historyLimit, DBPath: "test-session.db",
		},
		Experience: experience.Config{
			Name: "experience", Listen: "127.0.0.1:0", Standalone: true,
			Shared: sh, SharedPath: "test-experience.db",
		},
		Store: st, Shared: sh,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	cfg.Session.Store = cfg.Store
	cfg.Experience.Shared = cfg.Shared
	logger := log.New(os.Stdout, "[memory-test] ", log.LstdFlags)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sess := session.New(cfg.Session, logger)
	if err := sess.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sess.Shutdown)
	// experience 的整理池访问必须知道 session 地址。
	cfg.Experience.Peers = map[string]string{"session": sess.WsURL()}
	exp := experience.New(cfg.Experience, logger)
	if err := exp.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(exp.Shutdown)
	f := facade.New(facade.Config{
		Name: "memory", Listen: "127.0.0.1:0", Center: centerURL,
		Peers:             map[string]string{"session": sess.WsURL(), "experience": exp.WsURL()},
		HeartbeatInterval: 200 * time.Millisecond,
	}, logger)
	if err := f.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.Shutdown)
	return &testStack{Server: f, sess: sess, exp: exp, st: cfg.Store}
}

// ---------- 用例 ----------

// TestContextProjectsStoredHistory 验证基座组装：库里的历史按 seq 投影到消息链，
// 当前用户消息就是触发条目；experience 为空时没有【记忆】块（历史不再折叠进记忆文本）。
func TestContextProjectsStoredHistory(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	opened := caller.openWork(t, srv.WsURL(), "w-t1", "t1", "历史任务")
	caller.storeV2(t, srv.WsURL(), "w-t1", "t1", opened.TaskID, "user", "上一轮的问题", "reasoner:p1")
	caller.storeV2(t, srv.WsURL(), "w-t1", "t1", opened.TaskID, "assistant", "上一轮的答复", "reasoner:p2")
	current := caller.storeV2(t, srv.WsURL(), "w-t1", "t1", opened.TaskID, "user", "新一轮问题", "reasoner:p3")

	got := caller.contextFor(t, srv.WsURL(), "w-t1", protocol.Trigger{Kind: "user", Item: current.ItemID})
	if got.Task.ID != opened.TaskID {
		t.Fatalf("context 应返回 work_open 建的任务: %+v", got.Task)
	}
	var order []string
	for _, m := range got.Messages {
		if m.Role == "user" || m.Role == "assistant" {
			order = append(order, m.Content)
		}
	}
	want := []string{"上一轮的问题", "上一轮的答复", "新一轮问题"}
	if strings.Join(order, "|") != strings.Join(want, "|") {
		t.Fatalf("历史投影顺序不对: %v，期望 %v（messages=%+v）", order, want, got.Messages)
	}
	if len(got.Messages) == 0 || got.Messages[0].Role != "system" {
		t.Fatalf("第一条应是 memory 的任务提示 system: %+v", got.Messages)
	}
	if block := memoryBlockText(got.Messages); block != "" {
		t.Fatalf("没有经验时不应注入【记忆】块: %q", block)
	}
}

// TestRememberRecallInjection 验证长期记忆 v0 全链：
// remember 写入（幂等）→ context 的 experience 注入带「核心记忆」（priority>0，不看相关性）
// 与「相关记忆」（bigram 命中）；forget 软删后不再出现；忘记不存在的 id 报错。
func TestRememberRecallInjection(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 写入核心条目（身份）与普通条目（偏好）。
	rp := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "猫叫团子", Priority: 1})
	if !rp.OK {
		t.Fatalf("remember 失败: %s", rp.Error)
	}
	var rr protocol.RememberResult
	if err := protocol.DecodeRaw(rp.Data, &rr); err != nil || rr.ID <= 0 || !rr.Created {
		t.Fatalf("remember 结果不符: %+v err=%v", rr, err)
	}
	// 幂等：重复写入跳过。
	rp = caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "猫叫团子", Priority: 1})
	var rr2 protocol.RememberResult
	if err := protocol.DecodeRaw(rp.Data, &rr2); err != nil || rr2.Created || rr2.ID != rr.ID {
		t.Fatalf("remember 幂等不符: %+v err=%v", rr2, err)
	}
	if rp := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "偏好", Content: "主人喜欢喝美式咖啡，不加糖"}); !rp.OK {
		t.Fatalf("remember 失败: %s", rp.Error)
	}

	opened := caller.openWork(t, srv.WsURL(), "w-t3", "t3", "记忆任务")
	// context：最后一条 user 消息是“我明天想喝咖啡”——核心必须带，相关命中“咖啡”。
	q1 := caller.storeV2(t, srv.WsURL(), "w-t3", "t3", opened.TaskID, "user", "我明天想喝咖啡", "reasoner:q1")
	block := memoryBlockText(caller.contextFor(t, srv.WsURL(), "w-t3", protocol.Trigger{Kind: "user", Item: q1.ItemID}).Messages)
	for _, want := range []string{"（核心记忆）", "猫叫团子", "（相关记忆）", "美式咖啡"} {
		if !strings.Contains(block, want) {
			t.Fatalf("记忆块缺少 %q: %q", want, block)
		}
	}

	// forget 核心条目 → 新触发再 context 不再出现。
	if rp := caller.call(t, srv.WsURL(), "forget", protocol.ForgetPayload{ID: rr.ID}); !rp.OK {
		t.Fatalf("forget 失败: %s", rp.Error)
	}
	q2 := caller.storeV2(t, srv.WsURL(), "w-t3", "t3", opened.TaskID, "user", "今天写点什么代码", "reasoner:q2")
	if block := memoryBlockText(caller.contextFor(t, srv.WsURL(), "w-t3", protocol.Trigger{Kind: "user", Item: q2.ItemID}).Messages); strings.Contains(block, "团子") {
		t.Fatalf("forget 后不应再注入核心条目: %q", block)
	}

	// 忘记不存在的 id → 报错。
	if rp := caller.call(t, srv.WsURL(), "forget", protocol.ForgetPayload{ID: 99999}); rp.OK {
		t.Fatal("forget 不存在的 id 应报错")
	}
}

// memoryBlockText 返回消息链里记忆块的文本（无则空串）。
func memoryBlockText(messages []protocol.LLMMessage) string {
	for _, m := range messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, compose.MemoryPrefix) {
			return m.Content
		}
	}
	return ""
}
