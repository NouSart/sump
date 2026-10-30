package tests

// files 服务级测试：真 WS + 真 SQLite/文件；
// save（URL 下载 / base64 / 缺 filename）→ fetch（base64 回转）→ read（全文 / 截断 / 二进制拒绝 / 不存在）。

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

	"github.com/NouSart/sump/files/server"
	"github.com/NouSart/sump/files/store"
	"github.com/NouSart/sump/protocol"
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

// call 向 files 发一次跳转并等回执（短连接）。
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
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "files", "trace-file", protocol.JumpPayload{Action: action, Input: raw})
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

// startFiles 起真 files 服务（存根中心 + 临时库）。
func startFiles(t *testing.T, centerURL string, readMaxChars int) *server.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := log.New(os.Stdout, "[files-test] ", log.LstdFlags)
	srv := server.New(server.Config{
		Name: "files", Listen: "127.0.0.1:0", Center: centerURL,
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
		ReadMaxChars: readMaxChars,
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

// TestSaveFetchReadFlow 验证 save（URL/base64）→ fetch 回转 → read 全文，与错误场景。
func TestSaveFetchReadFlow(t *testing.T) {
	sc := startStubCenter(t)
	md := []byte("# 你好\n这是 Markdown 文件内容。\n")
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write(md)
	}))
	t.Cleanup(fileSrv.Close)
	srv := startFiles(t, sc.URL(), 4000)

	// save by URL（filename 必填）
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: fileSrv.URL + "/notes.md", Filename: "notes.md"})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID == "" || sr.Filename != "notes.md" || sr.Mime != "text/markdown" || sr.Size != int64(len(md)) {
		t.Fatalf("save 结果不符: %+v", sr)
	}

	// fetch → base64 回转一致（含文件名）
	rp2 := call(t, srv.WsURL(), "fetch", protocol.FileFetchPayload{ID: sr.ID})
	if !rp2.OK {
		t.Fatalf("fetch 失败: %s", rp2.Error)
	}
	var fr protocol.FileFetchResult
	if err := protocol.DecodeRaw(rp2.Data, &fr); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, md) || fr.Filename != "notes.md" || fr.Mime != "text/markdown" {
		t.Fatalf("fetch 内容不符: len=%d meta=%+v", len(got), fr)
	}

	// read 文本类 → 全文、未截断
	rp3 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID})
	if !rp3.OK {
		t.Fatalf("read 失败: %s", rp3.Error)
	}
	var rr protocol.FileReadResult
	if err := protocol.DecodeRaw(rp3.Data, &rr); err != nil {
		t.Fatal(err)
	}
	if rr.Text != string(md) || rr.Truncated || rr.Filename != "notes.md" {
		t.Fatalf("read 结果不符: %+v", rr)
	}

	// save by base64（无 mime → 内容嗅探）
	rp4 := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte("你好，世界")),
		Filename: "hi.txt",
	})
	if !rp4.OK {
		t.Fatalf("base64 save 失败: %s", rp4.Error)
	}

	// 二进制文件 read → 明确拒绝
	rp5 := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte{0x25, 0x50, 0x44, 0x46, 0x01}),
		Filename: "x.pdf", Mime: "application/pdf",
	})
	if !rp5.OK {
		t.Fatalf("pdf save 失败: %s", rp5.Error)
	}
	var sr5 protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp5.Data, &sr5); err != nil {
		t.Fatal(err)
	}
	if rp6 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr5.ID}); rp6.OK {
		t.Fatal("二进制文件 read 应报错")
	}

	// read / fetch 不存在的 id → 报错
	if rp7 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: "不存在"}); rp7.OK {
		t.Fatal("不存在的 id（read）应报错")
	}
	if rp8 := call(t, srv.WsURL(), "fetch", protocol.FileFetchPayload{ID: "不存在"}); rp8.OK {
		t.Fatal("不存在的 id（fetch）应报错")
	}

	// save 缺 filename → 报错
	if rp9 := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data: base64.StdEncoding.EncodeToString([]byte("x")),
	}); rp9.OK {
		t.Fatal("缺 filename 应报错")
	}
}

// TestReadTruncation 超长文本按字符数截断并置 truncated（“没给全”显式告知）。
func TestReadTruncation(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 10)
	full := "一二三四五六七八九十还有超出的部分"
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte(full)),
		Filename: "long.txt",
	})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	rp2 := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: sr.ID})
	if !rp2.OK {
		t.Fatalf("read 失败: %s", rp2.Error)
	}
	var rr protocol.FileReadResult
	if err := protocol.DecodeRaw(rp2.Data, &rr); err != nil {
		t.Fatal(err)
	}
	if !rr.Truncated || rr.Text != "一二三四五六七八九十" {
		t.Fatalf("截断不符: %+v", rr)
	}
}

// TestToolExposure：模型能 save（URL 入库）→ read/write/edit_word，但 fetch 不暴露——
// 它回的是 base64，进了模型上下文只是撑爆窗口。这条断言防的是"顺手把所有动作都开出去"。
func TestToolExposure(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	_ = srv
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
	for _, want := range []string{"save", "read", "write", "edit_word"} {
		if !tools[want] {
			t.Fatalf("files.%s 没暴露给模型; 实际暴露=%v", want, tools)
		}
	}
	if tools["fetch"] {
		t.Fatal("files.fetch 不该暴露给模型（base64 会撑爆上下文）")
	}
}
