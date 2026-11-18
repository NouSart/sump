package experience

// 经验服务的真进程级测试：真 WS + 真共享库，不依赖设置中心、不依赖外网。
// 覆盖：remember→inject 的核心/相关两节、空目录、本域设置声明、名片方法集、
// 独立模式（空 Center）可启动。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// startExperience 起一个独立模式（不连设置中心、Center 留空）的经验服务。
func startExperience(t *testing.T, cfg Config) (*Server, *shared.Store) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "experience.db")
	sh, err := shared.Open(path, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sh.Close() })

	cfg.Listen = "127.0.0.1:0"
	cfg.Standalone = true
	cfg.Shared = sh
	cfg.SharedPath = path
	if cfg.HeartbeatInterval == 0 {
		cfg.HeartbeatInterval = 200 * time.Millisecond
	}
	srv := New(cfg, log.New(io.Discard, "", 0))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv, sh
}

// dial 建立一条到 /spv2 的连接（调用方身份由各用例自己给）。
func dial(t *testing.T, url string) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// call 发一跳并交回回执 body；失败回执以 *spv2.Error 返回，交给用例自己判断。
func call(t *testing.T, c *spv2.Client, from spv2.Ref, work, method string, body any) ([]byte, error) {
	t.Helper()
	f, err := spv2.NewCall(work, from, spv2.ServiceRef("experience"), method, body)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// TestRememberThenInject 钉住记忆写入与注入四节的核心语义：
// 核心（priority>0）不看相关性、相关（bigram 兜底）按查询命中；没有会话原文。
func TestRememberThenInject(t *testing.T) {
	srv, _ := startExperience(t, Config{})
	c := dial(t, srv.WsURL())

	body, err := call(t, c, spv2.ServiceRef("reasoner"), "trace-remember", "remember", protocol.RememberPayload{
		Kind: "身份", Content: "用户喜欢简洁的中文回答", Priority: 1,
	})
	if err != nil {
		t.Fatalf("remember（核心）失败: %v", err)
	}
	var core protocol.RememberResult
	if err := protocol.DecodeRaw(body, &core); err != nil {
		t.Fatal(err)
	}
	if !core.Created || core.ID <= 0 {
		t.Fatalf("核心记忆应新建: %+v", core)
	}

	body, err = call(t, c, spv2.ServiceRef("reasoner"), "trace-remember", "remember", protocol.RememberPayload{
		Kind: "知识", Content: "项目使用 Go 语言开发", Salience: 0.6,
	})
	if err != nil {
		t.Fatalf("remember（相关）失败: %v", err)
	}
	var rel protocol.RememberResult
	if err := protocol.DecodeRaw(body, &rel); err != nil {
		t.Fatal(err)
	}
	if !rel.Created || rel.ID == core.ID {
		t.Fatalf("相关记忆应新建: %+v", rel)
	}

	body, err = call(t, c, spv2.ServiceRef("memory"), "trace-inject", "inject", protocol.InjectPayload{
		Viewer: "memory", Origin: "c-self", Query: "Go 语言",
	})
	if err != nil {
		t.Fatalf("inject 失败: %v", err)
	}
	var res protocol.InjectResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Core) != 1 || !strings.Contains(res.Core[0], "[身份] 用户喜欢简洁的中文回答") {
		t.Fatalf("核心节不符: %+v", res.Core)
	}
	if len(res.Relevant) != 1 || !strings.Contains(res.Relevant[0], "[知识] 项目使用 Go 语言开发") {
		t.Fatalf("相关节应按查询命中: %+v", res.Relevant)
	}
	if len(res.Experience) != 0 || len(res.Others) != 0 {
		t.Fatalf("没有经验网与目录时后两节应为空: %+v", res)
	}
}

// TestSharedSessionsEmpty 钉住空目录：没有发布过摘要时返回空列表，不是错误。
func TestSharedSessionsEmpty(t *testing.T) {
	srv, _ := startExperience(t, Config{})
	c := dial(t, srv.WsURL())

	body, err := call(t, c, spv2.ServiceRef("memory"), "trace-shared", "shared_sessions", protocol.SharedSessionsPayload{})
	if err != nil {
		t.Fatalf("shared_sessions 失败: %v", err)
	}
	var res protocol.SharedSessionsResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 0 {
		t.Fatalf("空目录应没有会话: %+v", res.Sessions)
	}
}

// TestSettingsSchemaOnlyOwnDomain 钉住设置声明：只有本域 8 个键，
// 不含 name / addr / center / db / context.history_limit。
func TestSettingsSchemaOnlyOwnDomain(t *testing.T) {
	srv, _ := startExperience(t, Config{})
	c := dial(t, srv.WsURL())

	body, err := call(t, c, spv2.ServiceRef("memory"), "trace-settings", "settings_schema", map[string]any{})
	if err != nil {
		t.Fatalf("settings_schema 失败: %v", err)
	}
	var settings []protocol.Setting
	if err := protocol.DecodeRaw(body, &settings); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"context.core_limit", "context.core_max_chars",
		"context.rel_limit", "context.rel_max_chars", "context.embedding",
		"share.sessions_limit", "share.digest_chars", "shared_db",
	}
	if len(settings) != len(want) {
		t.Fatalf("设置项应只声明本域 %d 个键: %+v", len(want), settings)
	}
	for i, key := range want {
		if settings[i].Key != key {
			t.Fatalf("第 %d 个设置键应为 %s，实际 %s", i+1, key, settings[i].Key)
		}
	}
	if !settings[len(want)-1].ReadOnly {
		t.Fatalf("shared_db 必须是只读项: %+v", settings[len(want)-1])
	}
	for _, s := range settings {
		if s.Key == "name" || s.Key == "addr" || s.Key == "center" || s.Key == "db" || s.Key == "context.history_limit" {
			t.Fatalf("设置声明出现非本域键 %q", s.Key)
		}
	}
}

// TestCardOnlyExperienceDomain 钉住名片：只有本域 method（含内部 inject / settings_schema），
// 没有会话动作；只有 dig / remember / shared_sessions 是模型工具。
func TestCardOnlyExperienceDomain(t *testing.T) {
	srv, _ := startExperience(t, Config{})
	c := dial(t, srv.WsURL())

	f, err := spv2.NewCall("trace-card", spv2.ServiceRef("test"), spv2.ServiceRef("experience"), "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		t.Fatalf("hello 失败: %v", err)
	}
	var hello spv2.HelloResponse
	if err := protocol.DecodeRaw(resp.Body, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Card.Name != "experience" {
		t.Fatalf("服务名应为 experience，实际 %q", hello.Card.Name)
	}

	want := []string{
		// 对外 method（provides 声明的 10 个）
		"remember", "forget", "consolidate", "note", "dig",
		"lineheads", "prune", "publish_session", "shared_sessions", "shared_forget",
		// 内部 method（注册但不进 provides）
		"inject", "settings_schema",
		// 骨架自带
		"configure", "hello", "health", "metrics",
	}
	allowed := make(map[string]bool, len(want))
	for _, name := range want {
		allowed[name] = true
	}
	tools := map[string]bool{"remember": true, "dig": true, "shared_sessions": true}
	got := make(map[string]bool, len(hello.Card.Methods))
	for _, m := range hello.Card.Methods {
		got[m.Name] = true
		if m.Tool != tools[m.Name] {
			t.Fatalf("%s 的 Tool 标记应保持旧实现（期望 %v）", m.Name, tools[m.Name])
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("名片缺少本域 method %s; 实际=%v", name, got)
		}
	}
	for name := range got {
		if !allowed[name] {
			t.Fatalf("名片出现非本域 method %s", name)
		}
	}
}

// TestStandaloneStartsWithEmptyCenter 独立模式（不连设置中心、Center 为空）也能启动，
// 且默认值对齐：服务名 experience、核心条数 3。
func TestStandaloneStartsWithEmptyCenter(t *testing.T) {
	srv, _ := startExperience(t, Config{Center: ""})
	if srv.WsURL() == "" {
		t.Fatal("独立模式启动后应已监听 /spv2")
	}
	if srv.cfg.Name != "experience" {
		t.Fatalf("默认服务名应为 experience，实际 %q", srv.cfg.Name)
	}
	if v := srv.SettingValue("context.core_limit"); v != "3" {
		t.Fatalf("默认核心条数应为 3，实际 %q", v)
	}
	if v := srv.SettingValue("share.digest_chars"); v != "120" {
		t.Fatalf("默认摘要字数应为 120，实际 %q", v)
	}
	// 没有名册也要能应答：真实一跳能回，才说明 /spv2 起来了。
	body, err := call(t, dial(t, srv.WsURL()), spv2.ServiceRef("test"), "trace-standalone",
		"shared_sessions", protocol.SharedSessionsPayload{})
	if err != nil {
		t.Fatalf("独立模式下应答失败: %v", err)
	}
	var res protocol.SharedSessionsResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
}
