// reasoner 设置项的**全链路样板**测试：三类声明（普通 / 秘密 / 只读）在注册名片里的形态、
// 普通项热改的真实生效（llm.* 重建客户端打向新 base；memory / images 调用打向新名字）、
// 秘密项落盘与重启带回、只读项拒绝。
package tests

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
)

// ---------- 辅助 ----------

// startSettingsReasoner 按给定配置起一个 reasoner（SecretFile 未指定时指到临时目录，
// 避免秘密文件写进仓库工作目录）。
func startSettingsReasoner(t *testing.T, sc *stubCenter, cfg server.Config) *server.Server {
	t.Helper()
	if cfg.Name == "" {
		cfg.Name = "reasoner"
	}
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:0"
	}
	if cfg.Center == "" {
		cfg.Center = sc.URL()
	}
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 100 * time.Millisecond
	}
	if cfg.SecretFile == "" {
		cfg.SecretFile = filepath.Join(t.TempDir(), "secret.json")
	}
	logger := log.New(os.Stdout, "[settings-test] ", log.LstdFlags)
	s := server.New(cfg, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	return s
}

// pushConfigure 下发一个设置项并返回响应（不预设成败：既测受理也测拒绝）。
func pushConfigure(t *testing.T, s *server.Server, p protocol.ConfigurePayload) protocol.ResponsePayload {
	t.Helper()
	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return jump(t, c, "configure", p, ctx)
}

// sendMessage 以 boss=stub-boss 发一条 user_message（payload 自定）并断言被受理。
func sendMessage(t *testing.T, s *server.Server, payload map[string]any) {
	t.Helper()
	c := dialReasoner(t, s)
	raw, err := protocol.EncodePayload(payload)
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "test", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "stub-boss"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
}

// registerOf 返回某个服务最近一次注册的 payload（重注册时取最后一次）。
func registerOf(t *testing.T, sc *stubCenter, name string) protocol.RegisterPayload {
	t.Helper()
	regs := sc.Registers()
	for i := len(regs) - 1; i >= 0; i-- {
		if regs[i].Name == name {
			return regs[i]
		}
	}
	t.Fatalf("没有 %s 的注册记录", name)
	return protocol.RegisterPayload{}
}

// settingOf 从注册名片里取一个设置声明。
func settingOf(t *testing.T, reg protocol.RegisterPayload, key string) protocol.Setting {
	t.Helper()
	for _, it := range reg.Settings {
		if it.Key == key {
			return it
		}
	}
	t.Fatalf("注册名片里没有设置项 %s", key)
	return protocol.Setting{}
}

// ---------- 聊天接口替身（普通 HTTP，非 WS） ----------

// stubChatAPI 是 OpenAI 兼容聊天接口的最小替身：记录 Authorization 头与请求体里的 model，
// 固定回一条 assistant 内容。
type stubChatAPI struct {
	ln      net.Listener
	srv     *http.Server
	content string // 固定回包内容

	mu     sync.Mutex
	auths  []string
	models []string
}

func startStubChatAPI(t *testing.T, content string) *stubChatAPI {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	a := &stubChatAPI{ln: ln, content: content}
	mux := http.NewServeMux()
	mux.HandleFunc("/chat/completions", a.handleChat)
	a.srv = &http.Server{Handler: mux}
	go func() { _ = a.srv.Serve(ln) }()
	t.Cleanup(func() { _ = a.srv.Close() })
	return a
}

func (a *stubChatAPI) URL() string { return "http://" + a.ln.Addr().String() }

// Calls 返回已处理的请求数。
func (a *stubChatAPI) Calls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.auths)
}

// Last 返回最近一次请求的 Authorization 头与 model（无请求时为空串）。
func (a *stubChatAPI) Last() (auth, model string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.auths) == 0 {
		return "", ""
	}
	return a.auths[len(a.auths)-1], a.models[len(a.models)-1]
}

func (a *stubChatAPI) handleChat(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	var req struct {
		Model string `json:"model"`
	}
	_ = json.Unmarshal(body, &req)
	a.mu.Lock()
	a.auths = append(a.auths, r.Header.Get("Authorization"))
	a.models = append(a.models, req.Model)
	a.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": a.content},
		}},
	})
}

// ---------- 用例 ----------

// TestSettingsDeclarations：注册名片带上三类新声明——
// llm.key 只有 Secret 标记（值不上线）；name/addr/center 只读且 Default = 启动值；
// llm.base/llm.model/memory/images 是普通项（Default = 启动参数）。
func TestSettingsDeclarations(t *testing.T) {
	sc := startStubCenter(t)
	startSettingsReasoner(t, sc, server.Config{
		Memory: "memory", Images: "images",
		LLMBase: "https://api.example.com", LLMModel: "model-x", LLMKey: "sk-flag",
	})
	reg := registerOf(t, sc, "reasoner")

	for key, want := range map[string]string{
		"llm.base":  "https://api.example.com",
		"llm.model": "model-x",
		"memory":    "memory",
		"images":    "images",
	} {
		it := settingOf(t, reg, key)
		if it.Secret || it.ReadOnly || it.Default != want {
			t.Fatalf("普通项 %s 声明不符: %+v", key, it)
		}
	}
	for key, want := range map[string]string{"name": "reasoner", "addr": "127.0.0.1:0", "center": sc.URL()} {
		it := settingOf(t, reg, key)
		if !it.ReadOnly || it.Secret || it.Default != want {
			t.Fatalf("只读项 %s 声明不符: %+v", key, it)
		}
	}
	// 秘密项：中心那侧只该看到「是不是秘密」，Default 必须为空（协议结构里根本没有值字段）。
	it := settingOf(t, reg, "llm.key")
	if !it.Secret || it.ReadOnly || it.Default != "" {
		t.Fatalf("秘密项 llm.key 声明不符: %+v", it)
	}
	// 原有三项仍在（保持不动）
	for _, key := range []string{"tool.notify_mode", "tool.concurrency", "tool.timeout_ms"} {
		_ = settingOf(t, reg, key)
	}
}

// TestLLMSettingsHotSwap：llm.base / llm.model 热改后，新轮次的请求真的打向新地址、
// 带新模型名与启动时经 flag 给出的 Key（客户端被按当前值重建）。
func TestLLMSettingsHotSwap(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	apiA := startStubChatAPI(t, "启动时的答复")
	apiB := startStubChatAPI(t, "热改后的答复")

	s := startSettingsReasoner(t, sc, server.Config{
		Memory: "memory", Images: "images",
		LLMBase: apiA.URL(), LLMModel: "model-a", LLMKey: "sk-flag",
	})
	mem.setReasonerURL(s.WsURL())

	if v := s.SettingValue("llm.base"); v != apiA.URL() {
		t.Fatalf("启动默认值应为 flag 值，实际 %q", v)
	}
	for _, p := range []protocol.ConfigurePayload{
		{Key: "llm.base", Value: apiB.URL(), Overridden: true},
		{Key: "llm.model", Value: "model-b", Overridden: true},
	} {
		if rp := pushConfigure(t, s, p); !rp.OK {
			t.Fatalf("configure(%s) 未被受理: %s", p.Key, rp.Error)
		}
	}
	if v := s.SettingValue("llm.base"); v != apiB.URL() {
		t.Fatalf("llm.base 生效值不符: %q", v)
	}
	if v := s.SettingValue("llm.model"); v != "model-b" {
		t.Fatalf("llm.model 生效值不符: %q", v)
	}

	sendMessage(t, s, map[string]any{"text": "热改后跑一轮"})
	select {
	case d := <-boss.delivered:
		if d.Text != "热改后的答复" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if n := apiA.Calls(); n != 0 {
		t.Fatalf("旧 base 不该再收到请求，实际 %d 次", n)
	}
	auth, model := apiB.Last()
	if auth != "Bearer sk-flag" {
		t.Fatalf("热改后仍应带启动 flag 的 Key，实际 Authorization=%q", auth)
	}
	if model != "model-b" {
		t.Fatalf("模型名应为热改后的值，实际 %q", model)
	}
	if n := apiB.Calls(); n != 1 {
		t.Fatalf("新 base 应收到 1 次请求，实际 %d 次", n)
	}
}

// TestMemoryImagesHotSwap：memory / images 热改后，后续调用真的打向新服务名——
// recall 到新记忆替身（旧的零召回）、fetch 到新图片替身（旧的零取图），且取图结果内联进 LLM 请求。
func TestMemoryImagesHotSwap(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem1 := startStubMemory(t, sc)
	mem2 := startStubMemoryNamed(t, sc, "memory2")
	img1 := startStubImages(t, sc)
	img2 := startStubImagesNamed(t, sc, "images2")
	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "换了服务也照常"}}}

	s := startSettingsReasoner(t, sc, server.Config{
		Memory: "memory", Images: "images", LLM: fake,
	})
	mem1.setReasonerURL(s.WsURL()) // 两个记忆都保留投递能力：靠 recall 计数区分究竟打给了谁
	mem2.setReasonerURL(s.WsURL())

	for _, p := range []protocol.ConfigurePayload{
		{Key: "memory", Value: "memory2", Overridden: true},
		{Key: "images", Value: "images2", Overridden: true},
	} {
		if rp := pushConfigure(t, s, p); !rp.OK {
			t.Fatalf("configure(%s) 未被受理: %s", p.Key, rp.Error)
		}
	}
	if v := s.SettingValue("memory"); v != "memory2" {
		t.Fatalf("memory 生效值不符: %q", v)
	}
	if v := s.SettingValue("images"); v != "images2" {
		t.Fatalf("images 生效值不符: %q", v)
	}

	sendMessage(t, s, map[string]any{"text": "看看这图", "images": []string{"img-1"}})
	select {
	case d := <-boss.delivered:
		if d.Text != "换了服务也照常" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if n := mem2.Recalls(); n < 1 {
		t.Fatalf("recall 应打向 memory2，实际 %d 次", n)
	}
	if n := mem1.Recalls(); n != 0 {
		t.Fatalf("旧记忆不该再被调用，实际 recall %d 次", n)
	}
	if f := img2.Fetched(); len(f) != 1 || f[0] != "img-1" {
		t.Fatalf("fetch 应打向 images2，实际 %+v", f)
	}
	if f := img1.Fetched(); len(f) != 0 {
		t.Fatalf("旧图片服务不该再被调用，实际 %+v", f)
	}
	if got := fake.LastRequest().Images["img-1"]; got != "data:image/png;base64,QUJD" {
		t.Fatalf("取图结果未内联进 LLM 请求: %+v", got)
	}
}

// TestSecretConfigureAndReadOnly：llm.key 下发 → 受理、服务侧拿到值、落盘到秘密文件，
// 但注册名片里只有「秘密」标记（值不上线）；只读项 addr 的改动被明确拒绝。
func TestSecretConfigureAndReadOnly(t *testing.T) {
	sc := startStubCenter(t)
	secretFile := filepath.Join(t.TempDir(), "secret.json")
	s := startSettingsReasoner(t, sc, server.Config{
		Memory: "memory", Images: "images",
		LLMBase: "https://api.example.com", LLMModel: "model-x",
		SecretFile: secretFile,
	})

	// 秘密下发：服务侧生效值表可读（对中心仍不回显）
	if rp := pushConfigure(t, s, protocol.ConfigurePayload{Key: "llm.key", Value: "sk-test", Overridden: true}); !rp.OK {
		t.Fatalf("秘密下发未被受理: %s", rp.Error)
	}
	if v := s.SettingValue("llm.key"); v != "sk-test" {
		t.Fatalf("服务侧应持有秘密，实际 %q", v)
	}
	// 落盘：秘密文件里能读到（明文，本机单用户场景，等同 .env）
	raw, err := os.ReadFile(secretFile)
	if err != nil {
		t.Fatalf("秘密文件不存在: %v", err)
	}
	var f struct {
		Secrets map[string]string `json:"secrets"`
	}
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("秘密文件解析失败: %v / %s", err, raw)
	}
	if f.Secrets["llm.key"] != "sk-test" {
		t.Fatalf("秘密文件内容不符: %s", raw)
	}
	// 中心那侧看不到值：注册名片里 llm.key 只有 Secret 标记、没有值（Default 为空）
	it := settingOf(t, registerOf(t, sc, "reasoner"), "llm.key")
	if !it.Secret || it.Default != "" {
		t.Fatalf("llm.key 声明不该带值: %+v", it)
	}

	// 只读项：改动被拒，生效值不变
	rp := pushConfigure(t, s, protocol.ConfigurePayload{Key: "addr", Value: "0.0.0.0:1", Overridden: true})
	if rp.OK {
		t.Fatal("只读项 addr 的改动应被拒绝")
	}
	if !strings.Contains(rp.Error, "只读") {
		t.Fatalf("拒绝理由应说明只读: %s", rp.Error)
	}
	if v := s.SettingValue("addr"); v != "127.0.0.1:0" {
		t.Fatalf("只读项生效值不该变: %q", v)
	}
}

// TestSecretSurvivesRestart：同一个秘密文件起两次服务。第二次 Start 从文件把 llm.key
// 带起来并重建客户端——重启后新轮次的请求带着同一个 Key 打到聊天 API；名片只报「有值」。
func TestSecretSurvivesRestart(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)
	api := startStubChatAPI(t, "重启后的答复")
	secretFile := filepath.Join(t.TempDir(), "secret.json")

	// 第一次：面板提交 Key（值落在服务自己的秘密文件里；中心那边没有）。
	s1 := startSettingsReasoner(t, sc, server.Config{
		Memory: "memory", Images: "images",
		LLMBase: api.URL(), LLMModel: "model-r", SecretFile: secretFile,
	})
	if rp := pushConfigure(t, s1, protocol.ConfigurePayload{Key: "llm.key", Value: "sk-test", Overridden: true}); !rp.OK {
		t.Fatalf("秘密下发未被受理: %s", rp.Error)
	}
	if v := s1.SettingValue("llm.key"); v != "sk-test" {
		t.Fatalf("第一次服务侧应持有秘密，实际 %q", v)
	}
	s1.Shutdown()

	// 第二次：同文件、无启动 Key —— Start 时加载秘密并重建客户端。
	s2 := startSettingsReasoner(t, sc, server.Config{
		Memory: "memory", Images: "images",
		LLMBase: api.URL(), LLMModel: "model-r", SecretFile: secretFile,
	})
	mem.setReasonerURL(s2.WsURL())
	if v := s2.SettingValue("llm.key"); v != "sk-test" {
		t.Fatalf("重启后秘密应已加载，实际 %q", v)
	}
	it := settingOf(t, registerOf(t, sc, "reasoner"), "llm.key")
	if !it.Secret || !it.Set || it.Default != "" {
		t.Fatalf("重启后的名片应只报「有值」: %+v", it)
	}

	// 行为：客户端确实带着文件里加载的 Key 重建了（新一轮请求打到聊天 API）。
	sendMessage(t, s2, map[string]any{"text": "重启后跑一轮"})
	select {
	case d := <-boss.delivered:
		if d.Text != "重启后的答复" {
			t.Fatalf("交付文本不符: %q", d.Text)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}
	auth, model := api.Last()
	if auth != "Bearer sk-test" {
		t.Fatalf("重启后应用文件里的 Key，实际 Authorization=%q", auth)
	}
	if model != "model-r" {
		t.Fatalf("模型名不符: %q", model)
	}
}
