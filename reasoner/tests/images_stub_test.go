package tests

// 图片链测试：user_message（含图片引用）→ 记忆往返 → 取图（images 服务）→ Fake LLM 收到内联图 → 交付带回会话标识。

import (
	"context"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

// stubImages 是图片服务替身：fetch 请求 → 返回预置 base64。
type stubImages struct {
	ln  net.Listener
	srv *http.Server

	mu      sync.Mutex
	name    string // 服务名（默认 images；服务名热改测试用别的名字）
	fetched []string
}

// startStubImages 起一个服务名为 "images" 的图片替身。
func startStubImages(t *testing.T, center *stubCenter) *stubImages {
	t.Helper()
	return startStubImagesNamed(t, center, "images")
}

// startStubImagesNamed 起一个自定义服务名的图片替身（服务名热改测试用）。
func startStubImagesNamed(t *testing.T, center *stubCenter, name string) *stubImages {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &stubImages{ln: ln, name: name}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", m.handleWS)
	m.srv = &http.Server{Handler: mux}
	go func() { _ = m.srv.Serve(ln) }()
	t.Cleanup(func() { _ = m.srv.Close() })

	center.mu.Lock()
	center.cards[name] = protocol.ServiceCard{
		Name:        name,
		Addr:        m.URL(),
		Description: "图片测试替身",
		Provides:    []protocol.Provide{{Action: "save"}, {Action: "fetch"}},
	}
	center.mu.Unlock()
	return m
}

func (m *stubImages) URL() string { return "ws://" + m.ln.Addr().String() + "/ws" }

// Fetched 返回已收到的 fetch 请求 id 快照。
func (m *stubImages) Fetched() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]string, len(m.fetched))
	copy(out, m.fetched)
	return out
}

func (m *stubImages) handleWS(w http.ResponseWriter, r *http.Request) {
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
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		if jp.Action != "fetch" {
			continue
		}
		var p protocol.ImageFetchPayload
		if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
			continue
		}
		m.mu.Lock()
		m.fetched = append(m.fetched, p.ID)
		m.mu.Unlock()

		// 预置图片："QUJD" = base64("ABC")。
		result := protocol.ImageFetchResult{ID: p.ID, Mime: "image/png", Data: "QUJD", Size: 3}
		resultData, err := protocol.EncodePayload(result)
		if err != nil {
			continue
		}
		resp, _ := protocol.NewResponse(env, m.name, protocol.ResponsePayload{OK: true, Data: resultData})
		c.send(resp)
	}
}

// TestImagePrefetch 验证图片引用在推理前被取回并内联（llm 请求的 Images 与消息 ImageIDs）。
func TestImagePrefetch(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	imgs := startStubImages(t, sc)

	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "图看完了"}}}

	logger := log.New(os.Stdout, "[img-test] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		Images:            "images",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(map[string]any{
		"text":            "看看这图",
		"conversation_id": "c-img",
		"images":          []string{"img-1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "stub-boss"
	resp, err := c.Call(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	var ack protocol.ResponsePayload
	if err := resp.DecodePayload(&ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Fatalf("user_message 未被受理: %s", ack.Error)
	}

	select {
	case d := <-boss.delivered:
		if d.Text != "图看完了" || d.ConversationID != "c-img" {
			t.Fatalf("交付不符: %+v", d)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if fake.CallCount() != 1 {
		t.Fatalf("Fake 调用次数 = %d, want 1", fake.CallCount())
	}
	req0 := fake.Requests[0]
	if req0.Images["img-1"] != "data:image/png;base64,QUJD" {
		t.Fatalf("图片未内联到请求: %+v", req0.Images)
	}
	var userMsg *llm.Message
	for i := range req0.Messages {
		if req0.Messages[i].Role == "user" {
			userMsg = &req0.Messages[i]
		}
	}
	if userMsg == nil || len(userMsg.ImageIDs) != 1 || userMsg.ImageIDs[0] != "img-1" {
		t.Fatalf("消息缺少图片引用: %+v", userMsg)
	}

	if f := imgs.Fetched(); len(f) != 1 || f[0] != "img-1" {
		t.Fatalf("fetch 调用不符: %+v", f)
	}
}
