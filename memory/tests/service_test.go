package tests

// memory 服务级测试：真 WS + 真 SQLite。
// 覆盖：store 落库 → recall 托管上下文 → 组装记忆 → resume 发回（from=memory）回路；
// 去重（任务链已有消息不重复注入）与记忆块替换。

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/memory/compose"
	"github.com/QAQ-awa-QAQ/sump/memory/longterm"
	"github.com/QAQ-awa-QAQ/sump/memory/server"
	"github.com/QAQ-awa-QAQ/sump/memory/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// ---------- 设置中心替身 ----------

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
	ln       net.Listener
	srv      *http.Server
	mu       sync.Mutex
	cards    map[string]protocol.ServiceCard
	settings map[string][]protocol.Setting
	conns    map[*stubConn]struct{}
}

var stubUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln, cards: map[string]protocol.ServiceCard{}, settings: map[string][]protocol.Setting{}, conns: map[*stubConn]struct{}{}}
	sc.cards["stub-center"] = protocol.ServiceCard{Name: "stub-center", Addr: sc.URL(), Description: "设置中心测试替身"}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sc.handleWS)
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/ws" }

// DeclaredSettings 返回某服务最近一次注册声明的设置项。
func (sc *stubCenter) DeclaredSettings(name string) []protocol.Setting {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.Setting(nil), sc.settings[name]...)
}

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
			sc.settings[reg.Name] = reg.Settings
			sc.conns[c] = struct{}{}
			roster := sc.snapshotLocked()
			targets := make([]*stubConn, 0, len(sc.conns))
			for cc := range sc.conns {
				targets = append(targets, cc)
			}
			sc.mu.Unlock()

			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true, Data: respData})
			c.send(resp)
			for _, cc := range targets {
				env, err := protocol.NewEnvelope(protocol.TypeRoster, "stub-center", "", "", roster)
				if err == nil {
					cc.send(env)
				}
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

// ---------- 调用方替身（扮演 reasoner） ----------

type stubCaller struct {
	ln      net.Listener
	srv     *http.Server
	resumes chan resumeRecord
}

type resumeRecord struct {
	from string
	env  protocol.Envelope
	tc   protocol.TaskContext
}

func startStubCaller(t *testing.T, centerURL string) *stubCaller {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &stubCaller{ln: ln, resumes: make(chan resumeRecord, 8)}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", c.handleWS)
	c.srv = &http.Server{Handler: mux}
	go func() { _ = c.srv.Serve(ln) }()
	t.Cleanup(func() { _ = c.srv.Close() })

	// 注册进设置中心（memory 的 resume 要经名册解析到我们）。
	reg, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	env, err := protocol.NewEnvelope(protocol.TypeRegister, "reasoner", "settings-center", "", protocol.RegisterPayload{
		Name: "reasoner", Addr: c.URL(), Description: "reasoner 测试替身",
	})
	if err != nil {
		t.Fatal(err)
	}
	resp, err := reg.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("caller 注册失败: %s", rp.Error)
	}
	return c
}

func (c *stubCaller) URL() string { return "ws://" + c.ln.Addr().String() + "/ws" }

func (c *stubCaller) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		if jp.Action == "resume" {
			var p protocol.ResumePayload
			if err := protocol.DecodeRaw(jp.Input, &p); err == nil {
				select {
				case c.resumes <- resumeRecord{from: env.From, env: env, tc: p.Context}:
				default:
				}
			}
			resp, _ := protocol.NewResponse(env, "reasoner", protocol.ResponsePayload{OK: true})
			_ = writeConn(ws, resp)
		}
	}
}

func writeConn(ws *websocket.Conn, env protocol.Envelope) error {
	data, err := protocol.Marshal(env)
	if err != nil {
		return err
	}
	return ws.WriteMessage(websocket.BinaryMessage, data)
}

// call 向 memory 发一次跳转并等回执（短连接）。
func (c *stubCaller) call(t *testing.T, memoryURL, action string, input any) protocol.ResponsePayload {
	t.Helper()
	conn, err := protocol.Dial(memoryURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "reasoner", "memory", "trace-test", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := conn.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// recallUntilReady 处理“caller 刚注册、memory 的 roster 可能还未更新”的时序：
// resume 解析失败会同步回错（未知目标服务），重试即可。
func (c *stubCaller) recallUntilReady(t *testing.T, memoryURL string, input protocol.RecallPayload) protocol.ResponsePayload {
	t.Helper()
	for i := 0; i < 20; i++ {
		rp := c.call(t, memoryURL, "recall", input)
		if rp.OK {
			return rp
		}
		if !strings.Contains(rp.Error, "未知目标服务") {
			t.Fatalf("recall 失败: %s", rp.Error)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("recall 重试超时：memory 名册中始终未见 reasoner")
	return protocol.ResponsePayload{}
}

// startMemory 起一个真 memory server（临时库）。
func startMemory(t *testing.T, centerURL string, historyLimit int) *server.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lt, err := longterm.Open(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	logger := log.New(os.Stdout, "[memory-test] ", log.LstdFlags)
	srv := server.New(server.Config{
		Name:              "memory",
		Listen:            "127.0.0.1:0",
		Center:            centerURL,
		HeartbeatInterval: 200 * time.Millisecond,
		Store:             st,
		Longterm:          lt,
		HistoryLimit:      historyLimit,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// ---------- 用例 ----------

// TestRecallResumeFlow 验证完整回路：store 两条历史 → recall → resume（from=memory，含记忆块）。
func TestRecallResumeFlow(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "t1", Role: "user", Content: "上一轮的问题"}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "t1", Role: "assistant", Content: "上一轮的答复"}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}

	task := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: "t1",
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "新一轮问题"},
		},
	}}
	rp := caller.recallUntilReady(t, srv.WsURL(), task)
	var ack struct {
		Status string `msgpack:"status"`
	}
	if err := protocol.DecodeRaw(rp.Data, &ack); err != nil || ack.Status != "accepted" {
		t.Fatalf("recall 回执不符: %+v", ack)
	}

	select {
	case r := <-caller.resumes:
		if r.from != "memory" {
			t.Fatalf("resume 来源不符: %q", r.from)
		}
		tc := r.tc
		if tc.ConversationID != "t1" || tc.Boss != "stub-boss" {
			t.Fatalf("上下文头不符: %+v", tc)
		}
		// 记忆块存在且在第一条 system 之后。
		if len(tc.Messages) < 3 || tc.Messages[1].Role != "system" || !strings.HasPrefix(tc.Messages[1].Content, compose.MemoryPrefix) {
			t.Fatalf("记忆块位置不符: %+v", tc.Messages)
		}
		if !strings.Contains(tc.Messages[1].Content, "上一轮的问题") || !strings.Contains(tc.Messages[1].Content, "上一轮的答复") {
			t.Fatalf("记忆内容缺失: %q", tc.Messages[1].Content)
		}
		// 任务链消息原样保留。
		last := tc.Messages[len(tc.Messages)-1]
		if last.Role != "user" || last.Content != "新一轮问题" {
			t.Fatalf("任务消息被改动: %+v", last)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到 resume")
	}
}

// TestRecallDedupAndReplace 验证：任务链已有的消息不重复注入；旧记忆块被替换而非叠加。
func TestRecallDedupAndReplace(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "t2", Role: "user", Content: "重复消息"}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}

	// 任务上下文里带着与历史尾部相同的消息 → 记忆中不应再出现它。
	task := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: "t2",
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "重复消息"},
		},
	}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}

	var first protocol.TaskContext
	select {
	case r := <-caller.resumes:
		first = r.tc
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到第一次 resume")
	}
	// 历史仅有一条“重复消息”，被去重后应为空 → 不应注入记忆块。
	for _, m := range first.Messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, compose.MemoryPrefix) {
			t.Fatalf("重复消息应被去重，不应有记忆块: %+v", first.Messages)
		}
	}

	// 模拟下一轮：上下文里已带旧记忆块 → 应被替换（同位置、单个），而非叠加。
	msgs := []protocol.Message{
		{Role: "system", Content: "sys"},
		{Role: "system", Content: compose.MemoryPrefix + "\n旧内容"},
		{Role: "user", Content: "再来一轮"},
	}
	task2 := protocol.RecallPayload{Context: protocol.TaskContext{ConversationID: "t2", Boss: "stub-boss", Messages: msgs}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task2); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	select {
	case r := <-caller.resumes:
		count := 0
		for _, m := range r.tc.Messages {
			if m.Role == "system" && strings.HasPrefix(m.Content, compose.MemoryPrefix) {
				count++
				if strings.Contains(m.Content, "旧内容") {
					t.Fatalf("旧记忆块应被替换: %q", m.Content)
				}
			}
		}
		if count != 1 {
			t.Fatalf("记忆块数量应为 1，实际 %d: %+v", count, r.tc.Messages)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到第二次 resume")
	}
}

// TestRememberRecallInjection 验证长期记忆 v0 全链：
// remember 写入（幂等）→ recall 组装带「核心记忆」（priority>0，不看相关性）与「相关记忆」（bigram 命中）；
// forget 软删后不再出现；忘记不存在的 id 报错。
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

	// recall：查询“我明天想喝咖啡”——核心必须带（不看相关性），相关命中“咖啡”（2 字词）。
	task := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: "t3",
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "我明天想喝咖啡"},
		},
	}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	var first protocol.TaskContext
	select {
	case r := <-caller.resumes:
		first = r.tc
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到 resume")
	}
	block := memoryBlockText(first.Messages)
	for _, want := range []string{"（核心记忆）", "猫叫团子", "（相关记忆）", "美式咖啡"} {
		if !strings.Contains(block, want) {
			t.Fatalf("记忆块缺少 %q: %q", want, block)
		}
	}

	// forget 核心条目 → 再 recall 不再出现。
	if rp := caller.call(t, srv.WsURL(), "forget", protocol.ForgetPayload{ID: rr.ID}); !rp.OK {
		t.Fatalf("forget 失败: %s", rp.Error)
	}
	task2 := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: "t3",
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "今天写点什么代码"},
		},
	}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task2); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	select {
	case r := <-caller.resumes:
		if strings.Contains(memoryBlockText(r.tc.Messages), "团子") {
			t.Fatalf("forget 后不应再注入核心条目: %q", memoryBlockText(r.tc.Messages))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到第二次 resume")
	}

	// 忘记不存在的 id → 报错。
	if rp := caller.call(t, srv.WsURL(), "forget", protocol.ForgetPayload{ID: 99999}); rp.OK {
		t.Fatal("forget 不存在的 id 应报错")
	}
}

// memoryBlockText 返回消息链里记忆块的文本（无则空串）。
func memoryBlockText(messages []protocol.Message) string {
	for _, m := range messages {
		if m.Role == "system" && strings.HasPrefix(m.Content, compose.MemoryPrefix) {
			return m.Content
		}
	}
	return ""
}
