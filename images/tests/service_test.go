package tests

// images 服务级测试：真 WS + 真 SQLite/文件；save（URL 下载 / base64）→ fetch（base64 回转）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/images/server"
	"github.com/QAQ-awa-QAQ/sump/images/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// ---------- 设置中心替身（精简） ----------

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
	ln  net.Listener
	srv *http.Server

	mu   sync.Mutex
	regs []protocol.RegisterPayload // 收到的注册（断言名片声明用）
}

// Registers 返回收到的注册快照（并发安全）。
func (sc *stubCenter) Registers() []protocol.RegisterPayload {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.RegisterPayload(nil), sc.regs...)
}

var stubUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln}
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
			if err := env.DecodePayload(&reg); err == nil {
				sc.mu.Lock()
				sc.regs = append(sc.regs, reg)
				sc.mu.Unlock()
			}
			roster := protocol.RosterPayload{Services: []protocol.ServiceCard{{Name: "settings-center", Addr: sc.URL()}}, Revision: 1}
			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: respData})
			c.send(resp)
		case protocol.TypeHeartbeat:
			// 忽略
		}
	}
}

// ---------- 调用辅助 ----------

// call 向 images 发一次跳转并等回执（短连接）。
func call(t *testing.T, url, action string, input any) protocol.ResponsePayload {
	t.Helper()
	conn, err := protocol.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "images", "trace-img", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
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

// ---------- 用例 ----------

// TestSaveFetchFlow 验证 save（URL 下载）→ fetch（base64 回转）与 base64 直存、错误场景。
func TestSaveFetchFlow(t *testing.T) {
	sc := startStubCenter(t)
	pngData := genPNG(t)
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	t.Cleanup(fileSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := log.New(os.Stdout, "[images-test] ", log.LstdFlags)
	srv := server.New(server.Config{
		Name: "images", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	// save by URL
	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: fileSrv.URL + "/a.png"})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID == "" || sr.Mime != "image/png" || sr.Size != int64(len(pngData)) {
		t.Fatalf("save 结果不符: %+v", sr)
	}

	// fetch → base64 回转一致
	rp2 := call(t, srv.WsURL(), "fetch", protocol.ImageFetchPayload{ID: sr.ID})
	if !rp2.OK {
		t.Fatalf("fetch 失败: %s", rp2.Error)
	}
	var fr protocol.ImageFetchResult
	if err := protocol.DecodeRaw(rp2.Data, &fr); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pngData) || fr.Mime != "image/png" {
		t.Fatalf("fetch 内容不符: len=%d mime=%s", len(got), fr.Mime)
	}

	// save by base64
	rp3 := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{
		Data: base64.StdEncoding.EncodeToString(pngData),
	})
	if !rp3.OK {
		t.Fatalf("base64 save 失败: %s", rp3.Error)
	}

	// fetch 不存在的 id
	rp4 := call(t, srv.WsURL(), "fetch", protocol.ImageFetchPayload{ID: "不存在"})
	if rp4.OK {
		t.Fatal("不存在的 id 应报错")
	}
}

// TestToolExposure：images 是图片索引（存 → 按 id 转 base64 发回），两个动作都对模型可用。
// 声明错一个 flag，模型就看不见这个能力——所以这里钉住暴露面。
func TestToolExposure(t *testing.T) {
	sc := startStubCenter(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{Name: "images", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond, Store: st}, log.New(os.Stdout, "[images-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sc.Registers()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	tools := map[string]bool{}
	for _, reg := range sc.Registers() {
		for _, p := range reg.Provides {
			if p.Tool {
				tools[p.Action] = true
			}
		}
	}
	for _, want := range []string{"save", "fetch"} {
		if !tools[want] {
			t.Fatalf("images.%s 没暴露给模型（Tool: true 缺失）; 实际暴露=%v", want, tools)
		}
	}
}
