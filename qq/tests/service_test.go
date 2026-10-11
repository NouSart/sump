package tests

// qq 服务级测试：真 qq 服务 + stub NapCat / stub center / stub reasoner / stub images。
// 覆盖：主人文本 → user_message（boss/cid 正确）→ deliver → 发回 QQ；
//       图片消息 → images.save → user_message 携带图片 id；
//       非主人私聊被拒绝（回拒绝语、不触发任务）。

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/qq/server"
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
	ln    net.Listener
	srv   *http.Server
	mu    sync.Mutex
	cards map[string]protocol.ServiceCard
}

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln, cards: map[string]protocol.ServiceCard{}}
	sc.cards["stub-center"] = protocol.ServiceCard{Name: "stub-center", Addr: sc.URL()}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sc.handleWS)
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/ws" }

func (sc *stubCenter) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
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
		switch env.Type {
		case protocol.TypeRegister:
			var reg protocol.RegisterPayload
			if err := env.DecodePayload(&reg); err != nil {
				continue
			}
			sc.mu.Lock()
			sc.cards[reg.Name] = reg.Card()
			roster := sc.snapshotLocked()
			sc.mu.Unlock()
			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "stub-center", protocol.ResponsePayload{OK: true, Data: respData})
			cc := &stubConn{ws: ws}
			cc.send(resp)
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

// ---------- stub NapCat（模拟正向 WS 服务端） ----------

type stubNapCat struct {
	ln      net.Listener
	srv     *http.Server
	writeMu sync.Mutex
	mu      sync.Mutex
	conn    *websocket.Conn
	sent    []map[string]any // 收到的 action 调用
}

func startStubNapCat(t *testing.T) *stubNapCat {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &stubNapCat{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", n.handleWS)
	n.srv = &http.Server{Handler: mux}
	go func() { _ = n.srv.Serve(ln) }()
	t.Cleanup(func() { _ = n.srv.Close() })
	return n
}

func (n *stubNapCat) URL() string { return "ws://" + n.ln.Addr().String() + "/ws" }

func (n *stubNapCat) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	n.mu.Lock()
	n.conn = ws
	n.mu.Unlock()
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		var m map[string]any
		if json.Unmarshal(data, &m) != nil {
			continue
		}
		n.mu.Lock()
		n.sent = append(n.sent, m)
		n.mu.Unlock()
		// 回动作响应（echo 配对）。
		resp := map[string]any{"status": "ok", "retcode": 0}
		if echo, _ := m["echo"].(string); echo != "" {
			resp["echo"] = echo
		}
		respData, _ := json.Marshal(resp)
		n.writeMu.Lock()
		_ = ws.WriteMessage(websocket.TextMessage, respData)
		n.writeMu.Unlock()
	}
}

func (n *stubNapCat) waitConn(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		ok := n.conn != nil
		n.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("qq 未连接到 stubNapCat")
}

func (n *stubNapCat) sendEvent(t *testing.T, ev map[string]any) {
	t.Helper()
	n.mu.Lock()
	conn := n.conn
	n.mu.Unlock()
	if conn == nil {
		t.Fatal("qq 尚未连接")
	}
	data, _ := json.Marshal(ev)
	n.writeMu.Lock()
	defer n.writeMu.Unlock()
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatal(err)
	}
}

// waitAction 等待某个 action 出现（返回最新一条匹配）。
func (n *stubNapCat) waitAction(t *testing.T, action string, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		for i := len(n.sent) - 1; i >= 0; i-- {
			if a, _ := n.sent[i]["action"].(string); a == action {
				m := n.sent[i]
				n.mu.Unlock()
				return m
			}
		}
		n.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待 action %s 超时", action)
	return nil
}

func privateTextEvent(uid int64, text string) map[string]any {
	return map[string]any{
		"post_type": "message", "message_type": "private", "sub_type": "friend",
		"user_id": uid, "self_id": 2578428650, "message_id": 100, "time": time.Now().Unix(),
		"raw_message": text, "font": 0,
		"message": []any{map[string]any{"type": "text", "data": map[string]any{"text": text}}},
	}
}

func privateImageEvent(uid int64, text, url string) map[string]any {
	segs := []any{}
	if text != "" {
		segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
	}
	segs = append(segs, map[string]any{"type": "image", "data": map[string]any{"url": url, "file": "x.png"}})
	return map[string]any{
		"post_type": "message", "message_type": "private", "sub_type": "friend",
		"user_id": uid, "self_id": 2578428650, "message_id": 101, "time": time.Now().Unix(),
		"raw_message": text, "font": 0, "message": segs,
	}
}

// ---------- stub reasoner ----------

type reasonerRequest struct {
	From    string
	Boss    string
	Payload map[string]any
}

type stubReasoner struct {
	ln   net.Listener
	srv  *http.Server
	mu   sync.Mutex
	reqs []reasonerRequest
}

func startStubReasoner(t *testing.T, center *stubCenter) *stubReasoner {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &stubReasoner{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", a.handleWS)
	a.srv = &http.Server{Handler: mux}
	go func() { _ = a.srv.Serve(ln) }()
	t.Cleanup(func() { _ = a.srv.Close() })

	center.mu.Lock()
	center.cards["reasoner"] = protocol.ServiceCard{Name: "reasoner", Addr: a.URL(), Description: "llm 测试替身"}
	center.mu.Unlock()
	return a
}

func (a *stubReasoner) URL() string { return "ws://" + a.ln.Addr().String() + "/ws" }

func (a *stubReasoner) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
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
		if jp.Action == "user_message" {
			var payload map[string]any
			_ = protocol.DecodeRaw(jp.Input, &payload)
			a.mu.Lock()
			a.reqs = append(a.reqs, reasonerRequest{From: env.From, Boss: env.Boss, Payload: payload})
			a.mu.Unlock()
			resp, _ := protocol.NewResponse(env, "reasoner", protocol.ResponsePayload{OK: true})
			c.send(resp)
		}
	}
}

func (a *stubReasoner) waitRequest(t *testing.T, timeout time.Duration) reasonerRequest {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		a.mu.Lock()
		if len(a.reqs) > 0 {
			r := a.reqs[0]
			a.mu.Unlock()
			return r
		}
		a.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待 user_message 超时")
	return reasonerRequest{}
}

func (a *stubReasoner) count() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.reqs)
}

// sendDeliver 模拟链终点向 qq 交付结果。
func (a *stubReasoner) sendDeliver(t *testing.T, qqURL, text, cid string) {
	t.Helper()
	raw, err := protocol.EncodePayload(protocol.DeliverPayload{Text: text, ConversationID: cid})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := protocol.Dial(qqURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	env, err := protocol.NewEnvelope(protocol.TypeJump, "reasoner", "qq", "trace-deliver", protocol.JumpPayload{Action: "deliver", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	env.Boss = "qq"
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("deliver 被拒: %s", rp.Error)
	}
}

// ---------- stub images ----------

type stubImages struct {
	ln    net.Listener
	srv   *http.Server
	mu    sync.Mutex
	saves []string
}

func startStubImages(t *testing.T, center *stubCenter) *stubImages {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	im := &stubImages{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", im.handleWS)
	im.srv = &http.Server{Handler: mux}
	go func() { _ = im.srv.Serve(ln) }()
	t.Cleanup(func() { _ = im.srv.Close() })

	center.mu.Lock()
	center.cards["images"] = protocol.ServiceCard{Name: "images", Addr: im.URL(), Description: "图片测试替身"}
	center.mu.Unlock()
	return im
}

func (im *stubImages) URL() string { return "ws://" + im.ln.Addr().String() + "/ws" }

func (im *stubImages) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
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
		if jp.Action != "save" {
			continue
		}
		var p protocol.ImageSavePayload
		_ = protocol.DecodeRaw(jp.Input, &p)
		im.mu.Lock()
		im.saves = append(im.saves, p.URL)
		im.mu.Unlock()
		result, _ := protocol.EncodePayload(protocol.ImageSaveResult{ID: "img-1", Mime: "image/png", Size: 3})
		resp, _ := protocol.NewResponse(env, "images", protocol.ResponsePayload{OK: true, Data: result})
		c.send(resp)
	}
}

func (im *stubImages) waitSave(t *testing.T, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		im.mu.Lock()
		if len(im.saves) > 0 {
			u := im.saves[0]
			im.mu.Unlock()
			return u
		}
		im.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待 images.save 超时")
	return ""
}

// ---------- 启动 qq 服务 ----------

func startQQ(t *testing.T, sc *stubCenter, nc *stubNapCat, owner string) *server.Server {
	t.Helper()
	logger := log.New(os.Stdout, "[qq-test] ", log.LstdFlags)
	srv := server.New(server.Config{
		Name:              "qq",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond,
		NapCatURL:         nc.URL(),
		Owner:             owner,
		Reasoner:          "reasoner",
		Images:            "images",
	}, logger)
	// t.Context()：随测试结束取消——napcat 连接循环需要活的 ctx，不能用 defer cancel()。
	if err := srv.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// ---------- 用例 ----------

// TestPrivateTextFlow 主人文本消息的完整桥接：user_message → deliver → send_msg。
func TestPrivateTextFlow(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	_ = startStubImages(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")

	nc.waitConn(t, 5*time.Second)
	nc.sendEvent(t, privateTextEvent(2271917353, "你好"))

	req := al.waitRequest(t, 5*time.Second)
	if req.Payload["text"] != "你好" {
		t.Fatalf("文本不符: %+v", req.Payload)
	}
	if req.Payload["conversation_id"] != "qq:private:2271917353" {
		t.Fatalf("会话标识不符: %+v", req.Payload)
	}
	if req.Boss != "qq" || req.From != "qq" {
		t.Fatalf("from/boss 不符: from=%s boss=%s", req.From, req.Boss)
	}

	al.sendDeliver(t, qq.WsURL(), "回复内容", "qq:private:2271917353")
	act := nc.waitAction(t, "send_msg", 5*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message"] != "回复内容" || params["message_type"] != "private" {
		t.Fatalf("send_msg 参数不符: %+v", params)
	}
	if int64(params["user_id"].(float64)) != 2271917353 {
		t.Fatalf("send_msg 目标不符: %+v", params)
	}
}

// TestPrivateImageFlow 图片消息：先存 images 服务，再随 user_message 传 id。
func TestPrivateImageFlow(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	im := startStubImages(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")

	nc.waitConn(t, 5*time.Second)
	nc.sendEvent(t, privateImageEvent(2271917353, "看看这图", "http://stub/img.png"))

	if u := im.waitSave(t, 5*time.Second); u != "http://stub/img.png" {
		t.Fatalf("images.save URL 不符: %s", u)
	}
	req := al.waitRequest(t, 5*time.Second)
	if req.Payload["text"] != "看看这图" {
		t.Fatalf("文本不符: %+v", req.Payload)
	}
	imgsAny, _ := req.Payload["images"].([]any)
	if len(imgsAny) != 1 || imgsAny[0] != "img-1" {
		t.Fatalf("图片 id 未随行: %+v", req.Payload)
	}
}

// TestNonOwnerRejected 非主人私聊：回拒绝语，不触发任务。
func TestNonOwnerRejected(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	_ = startStubImages(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")

	nc.waitConn(t, 5*time.Second)
	nc.sendEvent(t, privateTextEvent(999, "hi"))

	act := nc.waitAction(t, "send_msg", 5*time.Second)
	params, _ := act["params"].(map[string]any)
	if msg, _ := params["message"].(string); !strings.Contains(msg, "授权用户") {
		t.Fatalf("拒绝语不符: %+v", params)
	}
	time.Sleep(300 * time.Millisecond)
	if al.count() != 0 {
		t.Fatal("非主人消息不应触发任务")
	}
}
