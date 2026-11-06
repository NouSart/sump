package tests

// M3：假 DeepSeek 驱动的真进程端到端——settings-center + reasoner + memory + 假 boss。
// 零外网依赖：LLM 由本进程内的 OpenAI 兼容替身扮演；验证完全启动链与记忆注入。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// fakeDeepSeek 是 OpenAI 兼容的假 LLM（异步工具版剧本）：
// toolFirst=true 时：第 1 轮回 tool_call（echo——未提供，会被执行侧闸门拒绝；另一个 memory__remember 正常）、第 2 轮回 tool_call（wait）、其后回文本；
// toolFirst=false 时始终回文本。
type fakeDeepSeek struct {
	mu       sync.Mutex
	calls    int
	requests []map[string]any
	srv      *httptest.Server
}

func startFakeDeepSeek(t *testing.T, toolFirst bool) *fakeDeepSeek {
	t.Helper()
	f := &fakeDeepSeek{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)

		f.mu.Lock()
		f.calls++
		f.requests = append(f.requests, req)
		call := f.calls
		f.mu.Unlock()

		var msg map[string]any
		switch {
		case toolFirst && call == 1:
			// 第 1 轮并发派发两个调用：echo（未提供——验证执行侧闸门拒绝）与 memory__remember
			//（后者验证 memory 的 provides 自动变成 LLM 工具、reasoner 零改动）。
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_e2e",
						"type": "function",
						"function": map[string]any{
							"name":      "reasoner__echo",
							"arguments": `{"msg":"e2e"}`,
						},
					},
					map[string]any{
						"id":   "call_remember",
						"type": "function",
						"function": map[string]any{
							"name":      "memory__remember",
							"arguments": `{"kind":"偏好","content":"主人喜欢喝美式咖啡，不加糖"}`,
						},
					},
				},
			}
		case toolFirst && call == 2:
			// 异步工具：第 2 轮调 wait，阻塞到两个工具的结果都入账。
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   "call_wait",
					"type": "function",
					"function": map[string]any{
						"name":      "wait",
						"arguments": `{}`,
					},
				}},
			}
		case toolFirst && call == 3, toolFirst && call >= 5 && call%2 == 1, !toolFirst && call%2 == 1:
			// 收尾门禁要求交付前用 task_status 判定任务：脚本化的模型也照规矩来一次，
			// 否则 reasoner 会补两轮提醒（那是 reasoner 单测的事，这里只走正路）。
			// 工具模式下第 1 轮先跑工具（第 1、2 次调用），第 3 次判定；之后每轮都是"判定 + 答复"两跳。
			// 纯文本模式下每轮也是"判定 + 答复"两跳（奇数次判定，偶数次答复）。
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []any{
					map[string]any{
						"id":   "call_judge",
						"type": "function",
						"function": map[string]any{
							"name":      "task_status",
							"arguments": `{"status":"done"}`,
						},
					},
				},
			}
		default:
			msg = map[string]any{"role": "assistant", "content": "E2E 最终答复"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeDeepSeek) URL() string { return f.srv.URL }

func (f *fakeDeepSeek) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeDeepSeek) Requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.requests))
	copy(out, f.requests)
	return out
}

// e2eBoss 是测试进程内的“老板”替身：实现约定的 deliver 动作。
type e2eBoss struct {
	srv       *httptest.Server
	delivered chan string
}

func startE2EBoss(t *testing.T) *e2eBoss {
	t.Helper()
	b := &e2eBoss{delivered: make(chan string, 4)}
	srv := newStubServer(t, "e2e-boss", "e2e 老板替身", spv2.Method{Name: "deliver", Shape: spv2.ShapeCall})
	srv.Handle("deliver", func(_ context.Context, call *spv2.Call) error {
		var p struct {
			Text string `msgpack:"text"`
		}
		if err := call.Frame.DecodeBody(&p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		select {
		case b.delivered <- p.Text:
		default:
		}
		return call.Reply(map[string]any{"ok": true})
	})
	b.srv = serveStub(t, srv)
	return b
}

func (b *e2eBoss) URL() string { return wsURL(b.srv) }

// register 把 boss 名片注册进真设置中心（保持连接，接收 roster）。
func (b *e2eBoss) register(t *testing.T, centerURL string) *spv2.Client {
	t.Helper()
	c := dialSPV2(t, centerURL)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	call, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef("e2e-boss"), spv2.ServiceRef("settings-center"), "register", protocol.RegisterPayload{
		Name:        "e2e-boss",
		Addr:        b.URL(),
		Description: "e2e 老板替身",
		Provides:    []protocol.Provide{{Action: "deliver", Input: "{text}", Output: "回执"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, call, &roster); err != nil {
		t.Fatalf("boss 注册失败: %v", err)
	}
	return c
}

// TestInferChainE2E 真进程版全链（完全启动）：user_message → 记忆往返（recall → resume）
// → 工具派发（异步受理回执；echo 被闸门拒绝、memory__remember 成功）→ 自跳 → wait 等到结果 → 收尾轮 → deliver；
// 以预置会话历史验证记忆注入，并以第二轮对话验证长期记忆召回（remember 写入的条目被注入）。
func TestInferChainE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	alBin := buildService(t, "github.com/NouSart/sump/reasoner")
	memBin := buildService(t, "github.com/NouSart/sump/memory")

	fakeLLM := startFakeDeepSeek(t, true)
	boss := startE2EBoss(t)

	scPort := freePort(t)
	alPort := freePort(t)
	memPort := freePort(t)
	centerURL := spv2URL(scPort)
	reasonerURL := spv2URL(alPort)
	memoryURL := spv2URL(memPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)

	// memory 先启动（reasoner 需要从名册解析到它）。
	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memoryURL, 60*time.Second)

	// boss 先进名册，再启动 reasoner。
	bossClient := boss.register(t, centerURL)
	t.Cleanup(func() { _ = bossClient.Close() })

	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
		"-reflect=false", // 复盘会多花一次模型调用；这条用例只验推理链与工具派发
	)
	waitWSReady(t, reasonerURL, 60*time.Second)

	// 等名册齐全。
	obs := dialSPV2(t, centerURL)
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "memory", 10*time.Second)

	// 预置会话历史（上一轮的对话）——验证 recall 会把它注入 LLM 请求。
	mc := dialSPV2(t, memoryURL)
	ctx0, cancel0 := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel0()
	for _, sp := range []protocol.StorePayload{
		{ConversationID: "e2e-conv", Role: "user", Content: "上次谈话内容A"},
		{ConversationID: "e2e-conv", Role: "assistant", Content: "上次答复B"},
	} {
		rp := callJumpFrom(t, mc, "e2e", "memory", "store", sp, ctx0)
		if !rp.OK {
			t.Fatalf("预置 store 失败: %s", rp.Error)
		}
	}

	// 发起链：from 就是结果归属方，所以用 e2e-boss 身份发起。
	ac := dialSPV2(t, reasonerURL)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ack := callJumpFrom(t, ac, "e2e-boss", "reasoner", "user_message", map[string]any{"text": "你好", "conversation_id": "e2e-conv"}, ctx)
	if !ack.OK {
		t.Fatalf("user_message 未被受理: %s", ack.Error)
	}

	select {
	case text := <-boss.delivered:
		if text != "E2E 最终答复" {
			t.Fatalf("交付文本不符: %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	if fakeLLM.Calls() != 4 {
		t.Fatalf("假 DeepSeek 调用次数 = %d, want 4", fakeLLM.Calls())
	}
	reqs := fakeLLM.Requests()

	// 工具列表（显式声明制）：memory 的 remember（provides 里 tool:true）自动变成 LLM 工具——reasoner 零改动；
	// 未声明的动作（settings-center 的 ping/roster、qq/boss 的 deliver、链机制动作）一律不暴露。
	tools1, _ := reqs[0]["tools"].([]any)
	toolNames := map[string]bool{}
	for _, tl := range tools1 {
		tm, _ := tl.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		if name, ok := fn["name"].(string); ok {
			toolNames[name] = true
		}
	}
	if !toolNames["memory__remember"] {
		t.Fatalf("工具列表应含 memory__remember（显式声明自动暴露）: %+v", tools1)
	}
	if !toolNames["wait"] {
		t.Fatalf("工具列表应含内置 wait: %+v", tools1)
	}
	for _, forbidden := range []string{
		"settings-center__ping", "settings-center__roster", // 未声明 tool
		"memory__recall", "memory__store", // 链机制动作（即便声明也不暴露）
		"reasoner__echo", "reasoner__debug_jump", // 调试动作
		"e2e-boss__deliver", // 链终点动作
	} {
		if toolNames[forbidden] {
			t.Fatalf("不应暴露的工具 %s 出现在列表: %+v", forbidden, tools1)
		}
	}

	// 第一轮请求：记忆块（预置会话历史）已被注入。
	msgs1, _ := reqs[0]["messages"].([]any)
	foundMemory := false
	for _, m := range msgs1 {
		mm, _ := m.(map[string]any)
		if s, _ := mm["content"].(string); strings.Contains(s, "【记忆】") && strings.Contains(s, "上次谈话内容A") {
			foundMemory = true
		}
	}
	if !foundMemory {
		t.Fatalf("第一轮 LLM 请求应含注入的会话历史: %+v", msgs1)
	}

	// 第二轮请求：两个调用都已受理——回执（tool 消息）占住 tool_calls 的配对位。
	msgs2, _ := reqs[1]["messages"].([]any)
	foundAck, foundAckRemember := false, false
	for _, m := range msgs2 {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" {
			s, _ := mm["content"].(string)
			if strings.Contains(s, "已受理") && strings.Contains(s, "call_e2e") {
				foundAck = true
			}
			if strings.Contains(s, "已受理") && strings.Contains(s, "call_remember") {
				foundAckRemember = true
			}
		}
	}
	if !foundAck || !foundAckRemember {
		t.Fatalf("第二次 LLM 请求应包含两个调用的受理回执: %+v", msgs2)
	}

	// 第三轮请求（wait 返回后）：两个调用都已入账——echo 被拒（“不可用”错误），remember 成功（created）。
	msgs3, _ := reqs[2]["messages"].([]any)
	foundResult, foundRememberResult := false, false
	for _, m := range msgs3 {
		mm, _ := m.(map[string]any)
		if mm["role"] == "user" {
			s, _ := mm["content"].(string)
			if strings.Contains(s, "[工具结果]") && strings.Contains(s, "不可用") && strings.Contains(s, "reasoner__echo") {
				foundResult = true
			}
			if strings.Contains(s, "[工具结果]") && strings.Contains(s, "memory__remember") && strings.Contains(s, "created") {
				foundRememberResult = true
			}
		}
	}
	if !foundResult || !foundRememberResult {
		t.Fatalf("第三次 LLM 请求应含 echo 的拒绝（“不可用”）与 remember 的成功结果: %+v", msgs3)
	}

	// ---------- 第二轮对话：长期记忆召回（remember 的条目应被注入） ----------
	ack2 := callJumpFrom(t, ac, "e2e-boss", "reasoner", "user_message", map[string]any{"text": "给我推荐点美式咖啡吧", "conversation_id": "e2e-conv"}, ctx)
	if !ack2.OK {
		t.Fatalf("第二轮 user_message 未被受理: %s", ack2.Error)
	}
	select {
	case text := <-boss.delivered:
		if text != "E2E 最终答复" {
			t.Fatalf("第二轮交付文本不符: %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("超时：boss 未收到第二轮 deliver")
	}

	// 判定轮之后（总第 4 次）：task_status 的结果已作为工具消息回到上下文。
	msgsJudge, _ := reqs[3]["messages"].([]any)
	foundJudge := false
	for _, m := range msgsJudge {
		mm, _ := m.(map[string]any)
		if mm["role"] == "tool" && mm["tool_call_id"] == "call_judge" {
			foundJudge = true
		}
	}
	if !foundJudge {
		t.Fatalf("判定轮的请求应含 task_status 的工具结果（tool_call_id=call_judge）: %+v", msgsJudge)
	}

	// 第二轮的第一轮请求（总第 5 次）：记忆块应含「相关记忆」与 remember 写入的内容。
	if fakeLLM.Calls() != 6 {
		t.Fatalf("假 DeepSeek 调用次数 = %d, want 6", fakeLLM.Calls())
	}
	reqs = fakeLLM.Requests()
	msgs4, _ := reqs[4]["messages"].([]any)
	foundRelevant := false
	for _, m := range msgs4 {
		mm, _ := m.(map[string]any)
		if s, _ := mm["content"].(string); strings.Contains(s, "（相关记忆）") && strings.Contains(s, "美式咖啡") {
			foundRelevant = true
		}
	}
	if !foundRelevant {
		t.Fatalf("第二轮请求应含长期记忆召回（相关记忆节）: %+v", msgs4)
	}
}
