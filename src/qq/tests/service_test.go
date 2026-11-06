package tests

// qq 服务级测试：真 qq 服务 + stub NapCat / stub center / stub reasoner / stub images。
// 覆盖：主人文本 → user_message（boss/cid 正确）→ deliver → 发回 QQ；
//       图片消息 → images.save → user_message 携带图片 id；
//       非主人私聊被拒绝（回拒绝语、不触发任务）。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/qq/server"
	"github.com/NouSart/sump/spv2"
)

// ---------- 设置中心替身（SPv2：注册是 call、名册是 event）----------

type stubCenter struct {
	srv  *spv2.Server
	http *httptest.Server

	mu    sync.Mutex
	cards map[string]protocol.ServiceCard
	calls map[string]*spv2.Call
}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "settings-center", Actor: spv2.ServiceRef("settings-center"),
		Version: "stub/1", Methods: []spv2.Method{
			{Name: "register", Shape: spv2.ShapeCall},
			{Name: "list_settings", Shape: spv2.ShapeCall},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{
		srv: srv, cards: map[string]protocol.ServiceCard{},
		calls: map[string]*spv2.Call{},
	}
	srv.Handle("register", sc.handleRegister)
	srv.Handle("list_settings", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(protocol.SettingsResult{})
	})
	sc.http = httptest.NewServer(srv.Handler())
	t.Cleanup(sc.http.Close)
	sc.cards["settings-center"] = protocol.ServiceCard{Name: "settings-center", Addr: sc.URL(), Description: "设置中心替身"}
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

// registerService 用一个原生 call 把替身登记进名册（地址就是替身自己的 /spv2）。
func registerService(t *testing.T, sc *stubCenter, name, addr string) {
	t.Helper()
	c, err := spv2.Dial(context.Background(), sc.URL())
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

// ---------- stub NapCat（模拟正向 WS 服务端；OneBot 自己的协议，不是 SUMP）----------

var wsUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

type stubNapCat struct {
	ln            net.Listener
	srv           *http.Server
	writeMu       sync.Mutex
	mu            sync.Mutex
	conn          *websocket.Conn
	sent          []map[string]any                         // 收到的 action 调用
	failSend      bool                                     // 测试用：send_msg 一律回失败
	failGroupInfo bool                                     // 测试用：get_group_info 一律失败（模拟网关不支持）
	replyShape    func(action string, resp map[string]any) // 测试用：改响应形状（如去掉 status）
}

// setReplyShape 换响应形状（并发安全：读侧在 handleWS 的锁里）。
func (n *stubNapCat) setReplyShape(fn func(action string, resp map[string]any)) {
	n.mu.Lock()
	n.replyShape = fn
	n.mu.Unlock()
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
		n.mu.Lock()
		failSend := n.failSend
		failGroup := n.failGroupInfo
		shape := n.replyShape
		n.mu.Unlock()
		action, _ := m["action"].(string)
		if failSend && action == "send_msg" {
			resp = map[string]any{"status": "failed", "retcode": 100, "message": "stub send failure"}
		}
		if action == "get_group_info" {
			if failGroup {
				resp = map[string]any{"status": "failed", "retcode": 1400, "message": "stub get_group_info failure"}
			} else {
				// 群名与成员数（= 消息的扩散面），qq 会把它们标进来源标注。
				resp["data"] = map[string]any{
					"group_id": 67890, "group_name": "测试群", "member_count": 7, "max_member_count": 200,
				}
			}
		}
		if shape != nil {
			shape(action, resp)
		}
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

func privateFileEvent(uid int64, text, url, name string) map[string]any {
	segs := []any{}
	if text != "" {
		segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
	}
	segs = append(segs, map[string]any{"type": "file", "data": map[string]any{"url": url, "file": name}})
	return map[string]any{
		"post_type": "message", "message_type": "private", "sub_type": "friend",
		"user_id": uid, "self_id": 2578428650, "message_id": 102, "time": time.Now().Unix(),
		"raw_message": text, "font": 0, "message": segs,
	}
}

// ---------- stub reasoner ----------

type reasonerRequest struct {
	From    string
	Boss    string // 原生帧没有单独的 boss：来源即结果归属（入口服务把 from 设成自己）
	Payload map[string]any
}

type stubReasoner struct {
	srv  *spv2.Server
	http *httptest.Server
	mu   sync.Mutex
	reqs []reasonerRequest
}

func startStubReasoner(t *testing.T, center *stubCenter) *stubReasoner {
	t.Helper()
	a := &stubReasoner{}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "reasoner", Actor: spv2.ServiceRef("reasoner"),
		Version: "stub/1", Methods: []spv2.Method{{Name: "user_message", Shape: spv2.ShapeCall}},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.srv = srv
	srv.Handle("user_message", func(_ context.Context, call *spv2.Call) error {
		var payload map[string]any
		_ = call.Frame.DecodeBody(&payload)
		from := call.Frame.From.ID
		a.mu.Lock()
		a.reqs = append(a.reqs, reasonerRequest{From: from, Boss: from, Payload: payload})
		a.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	a.http = httptest.NewServer(srv.Handler())
	t.Cleanup(a.http.Close)
	registerService(t, center, "reasoner", a.URL())
	return a
}

func (a *stubReasoner) URL() string {
	return "ws" + strings.TrimPrefix(a.http.URL, "http") + "/spv2"
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

// snapshot 返回已收到的全部 user_message（按到达顺序，浅拷贝）。
func (a *stubReasoner) snapshot() []reasonerRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]reasonerRequest, len(a.reqs))
	copy(out, a.reqs)
	return out
}

// sendDeliver 模拟链终点向 qq 交付结果（原生 deliver；原生帧里来源即结果归属）。
func (a *stubReasoner) sendDeliver(t *testing.T, qqURL, text, cid string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, qqURL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	call, err := spv2.NewCall("trace-deliver", spv2.ServiceRef("reasoner"), spv2.ServiceRef("qq"), "deliver",
		protocol.DeliverPayload{Text: text, ConversationID: cid})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call); err != nil {
		t.Fatalf("deliver 被拒: %v", err)
	}
}

// ---------- stub images ----------

type stubImages struct {
	srv   *spv2.Server
	http  *httptest.Server
	mu    sync.Mutex
	saves []string
}

func startStubImages(t *testing.T, center *stubCenter) *stubImages {
	t.Helper()
	im := &stubImages{}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "images", Actor: spv2.ServiceRef("images"),
		Version: "stub/1", Methods: []spv2.Method{{Name: "save", Shape: spv2.ShapeCall}},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	im.srv = srv
	srv.Handle("save", func(_ context.Context, call *spv2.Call) error {
		var p protocol.ImageSavePayload
		if err := call.Frame.DecodeBody(&p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		im.mu.Lock()
		im.saves = append(im.saves, p.URL)
		im.mu.Unlock()
		return call.Reply(protocol.ImageSaveResult{ID: "img-1", Mime: "image/png", Size: 3})
	})
	im.http = httptest.NewServer(srv.Handler())
	t.Cleanup(im.http.Close)
	registerService(t, center, "images", im.URL())
	return im
}

func (im *stubImages) URL() string {
	return "ws" + strings.TrimPrefix(im.http.URL, "http") + "/spv2"
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

// ---------- stub files ----------

type fileSave struct {
	URL      string
	Filename string
}

type stubFiles struct {
	srv   *spv2.Server
	http  *httptest.Server
	mu    sync.Mutex
	saves []fileSave
}

func startStubFiles(t *testing.T, center *stubCenter) *stubFiles {
	return startStubFilesNamed(t, center, "files")
}

// startStubFilesNamed 同上，但可指定注册名（测 files 设置项热改用）。
func startStubFilesNamed(t *testing.T, center *stubCenter, name string) *stubFiles {
	t.Helper()
	sf := &stubFiles{}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: name, Actor: spv2.ServiceRef(name),
		Version: "stub/1", Methods: []spv2.Method{{Name: "save", Shape: spv2.ShapeCall}},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	sf.srv = srv
	srv.Handle("save", func(_ context.Context, call *spv2.Call) error {
		var p protocol.FileSavePayload
		if err := call.Frame.DecodeBody(&p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		sf.mu.Lock()
		sf.saves = append(sf.saves, fileSave{URL: p.URL, Filename: p.Filename})
		sf.mu.Unlock()
		return call.Reply(protocol.FileSaveResult{ID: "file-1", Filename: p.Filename, Mime: "text/markdown", Size: 3})
	})
	sf.http = httptest.NewServer(srv.Handler())
	t.Cleanup(sf.http.Close)
	registerService(t, center, name, sf.URL())
	return sf
}

func (sf *stubFiles) URL() string {
	return "ws" + strings.TrimPrefix(sf.http.URL, "http") + "/spv2"
}

// savesLen 返回已收到的 save 条数（并发安全）。
func (sf *stubFiles) savesLen() int {
	sf.mu.Lock()
	defer sf.mu.Unlock()
	return len(sf.saves)
}

func (sf *stubFiles) waitSave(t *testing.T, timeout time.Duration) fileSave {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sf.mu.Lock()
		if len(sf.saves) > 0 {
			s := sf.saves[0]
			sf.mu.Unlock()
			return s
		}
		sf.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待 files.save 超时")
	return fileSave{}
}

// ---------- 启动 qq 服务 ----------

func startQQ(t *testing.T, sc *stubCenter, nc *stubNapCat, owner string) *server.Server {
	t.Helper()
	return startQQWithLog(t, sc, nc, owner, os.Stdout)
}

// startQQWithLog 同 startQQ，但把日志写到指定 writer（断言"失败有信号"时用）。
func startQQWithLog(t *testing.T, sc *stubCenter, nc *stubNapCat, owner string, out io.Writer) *server.Server {
	t.Helper()
	logger := log.New(out, "[qq-test] ", log.LstdFlags)
	srv := server.New(server.Config{
		Name:              "qq",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond,
		NapCatURL:         nc.URL(),
		Owner:             owner,
		Reasoner:          "reasoner",
		Images:            "images",
		// 秘密项落盘：指到临时目录，别写进仓库工作目录
		SecretFile: filepath.Join(t.TempDir(), "qq-secret.json"),
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

// TestPrivateFileFlow 文件消息：先存 files 服务（URL+文件名），占位文本带 id，再随 user_message 传文件 id。
func TestPrivateFileFlow(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sf := startStubFiles(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")

	nc.waitConn(t, 5*time.Second)
	nc.sendEvent(t, privateFileEvent(2271917353, "看看这个", "http://stub/report.md", "report.md"))

	save := sf.waitSave(t, 5*time.Second)
	if save.URL != "http://stub/report.md" || save.Filename != "report.md" {
		t.Fatalf("files.save 不符: %+v", save)
	}
	req := al.waitRequest(t, 5*time.Second)
	text, _ := req.Payload["text"].(string)
	if !strings.Contains(text, "[文件: report.md, id=file-1]") {
		t.Fatalf("占位文本不符（须带文件名与 id）: %+v", req.Payload)
	}
	filesAny, _ := req.Payload["files"].([]any)
	if len(filesAny) != 1 || filesAny[0] != "file-1" {
		t.Fatalf("文件 id 未随行: %+v", req.Payload)
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
