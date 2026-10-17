package tests

// M4 e2e：真进程全链——stub NapCat → qq → images → center → reasoner → memory → 假 DeepSeek → 回发 QQ。
// 场景：主人私聊发图 + 文本 → 最终回复经 send_msg 发回；图片经 images 存取、reasoner 取图内联给模型。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// ---------- stub NapCat（模拟正向 WS 服务端） ----------

type napcatStub struct {
	ln      net.Listener
	srv     *http.Server
	writeMu sync.Mutex
	mu      sync.Mutex
	conn    *websocket.Conn
	sent    []map[string]any
}

func startNapCatStub(t *testing.T) *napcatStub {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	n := &napcatStub{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", n.handleWS)
	n.srv = &http.Server{Handler: mux}
	go func() { _ = n.srv.Serve(ln) }()
	t.Cleanup(func() { _ = n.srv.Close() })
	return n
}

func (n *napcatStub) URL() string { return "ws://" + n.ln.Addr().String() + "/ws" }

func (n *napcatStub) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := bossUpgrader.Upgrade(w, r, nil)
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

func (n *napcatStub) waitConn(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		ok := n.conn != nil
		n.mu.Unlock()
		if ok {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("qq 未连接到 stubNapCat")
}

func (n *napcatStub) sendEvent(t *testing.T, ev map[string]any) {
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

// waitSendMsg 等待一条 send_msg 调用。
func (n *napcatStub) waitSendMsg(t *testing.T, timeout time.Duration) map[string]any {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n.mu.Lock()
		for i := len(n.sent) - 1; i >= 0; i-- {
			if a, _ := n.sent[i]["action"].(string); a == "send_msg" {
				m := n.sent[i]
				n.mu.Unlock()
				return m
			}
		}
		n.mu.Unlock()
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("等待 send_msg 超时")
	return nil
}

func qqPrivateEvent(uid int64, text, imageURL string) map[string]any {
	segs := []any{}
	if text != "" {
		segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
	}
	if imageURL != "" {
		segs = append(segs, map[string]any{"type": "image", "data": map[string]any{"url": imageURL, "file": "x.png"}})
	}
	return map[string]any{
		"post_type": "message", "message_type": "private", "sub_type": "friend",
		"user_id": uid, "self_id": 2578428650, "message_id": 1, "time": time.Now().Unix(),
		"raw_message": text, "font": 0, "message": segs,
	}
}

// ---------- 用例 ----------

// TestQQFullChainE2E 主人私聊（图+文）→ 记忆/取图/推理 → deliver → send_msg 回发。
func TestQQFullChainE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	memBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/memory")
	imgBin := buildServiceNamed(t, "github.com/QAQ-awa-QAQ/sump/images", "sump-images")
	alBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/reasoner")
	qqBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/qq")

	fakeLLM := startFakeDeepSeek(t, false) // 纯文本模式：一次调用即回复
	nc := startNapCatStub(t)

	// 图片素材：httptest 提供一张 2x2 PNG（images 服务会来下载）。
	var pngBuf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{G: 255, A: 255})
	if err := png.Encode(&pngBuf, img); err != nil {
		t.Fatal(err)
	}
	pngSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngBuf.Bytes())
	}))
	t.Cleanup(pngSrv.Close)

	scPort := freePort(t)
	memPort := freePort(t)
	imgPort := freePort(t)
	alPort := freePort(t)
	qqPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	memURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
	imgURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", imgPort)
	alURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", alPort)
	qqURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", qqPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)

	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memURL, 60*time.Second)

	startProc(t, imgBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", imgPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "images.db"),
		"-dir", t.TempDir(),
	)
	waitWSReady(t, imgURL, 60*time.Second)

	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
	)
	waitWSReady(t, alURL, 60*time.Second)

	startProc(t, qqBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", qqPort),
		"-center", centerURL,
		"-napcat", nc.URL(),
		"-owner", "2271917353",
	)
	waitWSReady(t, qqURL, 60*time.Second)

	// 等名册齐全。
	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	for _, name := range []string{"memory", "images", "reasoner", "qq"} {
		waitRosterHas(t, obs, name, 60*time.Second)
	}

	// 主人私聊发图 + 文本。
	nc.waitConn(t, 60*time.Second)
	nc.sendEvent(t, qqPrivateEvent(2271917353, "看看这图", pngSrv.URL+"/a.png"))

	// 最终回复经 send_msg 发回。
	act := nc.waitSendMsg(t, 60*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message"] != "E2E 最终答复" {
		t.Fatalf("回复文本不符: %+v", params)
	}
	if int64(params["user_id"].(float64)) != 2271917353 || params["message_type"] != "private" {
		t.Fatalf("回复目标不符: %+v", params)
	}

	// 假 DeepSeek 收到的请求应含内联图片（多模态 content 数组）。
	// 假 DeepSeek 收到两次调用：先 task_status 判定任务（收尾门禁），再给答复。
	if fakeLLM.Calls() != 2 {
		t.Fatalf("假 DeepSeek 调用次数 = %d, want 2", fakeLLM.Calls())
	}
	reqs := fakeLLM.Requests()
	msgs, _ := reqs[0]["messages"].([]any)
	foundImage := false
	foundText := false
	for _, m := range msgs {
		mm, _ := m.(map[string]any)
		content, ok := mm["content"].([]any)
		if !ok {
			continue
		}
		for _, part := range content {
			pp, _ := part.(map[string]any)
			if pp["type"] == "image_url" {
				u, _ := pp["image_url"].(map[string]any)["url"].(string)
				if strings.HasPrefix(u, "data:image/png;base64,") {
					foundImage = true
				}
			}
			if pp["type"] == "text" {
				if s, _ := pp["text"].(string); strings.Contains(s, "看看这图") {
					foundText = true
				}
			}
		}
	}
	if !foundImage || !foundText {
		t.Fatalf("多模态请求不符: image=%v text=%v（%+v）", foundImage, foundText, msgs)
	}
}
