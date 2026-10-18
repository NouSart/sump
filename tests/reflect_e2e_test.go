// 复盘链的数据流转 e2e：交付之后，一段做过的事要真的变成经验网里的条目。
//
// 这条用例守的是一个很容易"看起来做了、其实没接上"的地方：复盘是后台异步的
// （交付完才跑），任何一环断了都不会有人报错——
//
//	交付 → 复盘（llm 服务自己的第二次补全）→ 解析 JSON → Fire note → 记忆服务落网
//
// 所以这里不看日志、不看返回值，只看**结果**：跑完之后经验网里能不能翻到那条经验。
// 复盘请求本身由假 LLM 按系统提示识别（真 LLM 在这条链里没有可断言的行为）。
package tests

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

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// reflectNote 是假 LLM 在复盘时写回的那条经验。
const reflectNote = "e2e 复盘经验：并发写要带会话内单调序号，否则落库顺序会乱"

// reflectFakeLLM 是"会复盘"的假 DeepSeek：
// 任务轮次按脚本产出（判定 → 答复），复盘轮次按系统提示识别并吐一段 JSON。
type reflectFakeLLM struct {
	srv *httptest.Server

	mu      sync.Mutex
	calls   int      // 非复盘调用数（任务轮次）
	loads   int      // 复盘调用数
	prompts []string // 复盘请求的 user 提示词（供断言流水里真的带上了这次任务）
}

func startReflectFakeLLM(t *testing.T) *reflectFakeLLM {
	t.Helper()
	f := &reflectFakeLLM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
			MaxTokens int `json:"max_tokens"`
		}
		_ = json.Unmarshal(body, &req)

		isReflect := false
		for _, m := range req.Messages {
			if m.Role == "system" && strings.Contains(m.Content, "复盘器") {
				isReflect = true
				break
			}
		}

		var msg map[string]any
		if isReflect {
			f.mu.Lock()
			f.loads++
			for _, m := range req.Messages {
				if m.Role == "user" {
					f.prompts = append(f.prompts, m.Content)
				}
			}
			f.mu.Unlock()
			// 故意包一层 ```json 围栏：真模型常这么干，解析要能吃下。
			msg = map[string]any{
				"role":    "assistant",
				"content": "```json\n{\"notes\":[{\"content\":\"" + reflectNote + "\",\"kind\":\"坑\"}]}\n```",
			}
		} else {
			f.mu.Lock()
			f.calls++
			n := f.calls
			f.mu.Unlock()
			if n%2 == 1 {
				// 收尾门禁：交付前必须先判定任务完成情况。
				msg = map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":       "call_judge",
						"type":     "function",
						"function": map[string]any{"name": "task_status", "arguments": `{"status":"done"}`},
					}},
				}
			} else {
				msg = map[string]any{"role": "assistant", "content": "E2E 复盘链路最终答复"}
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *reflectFakeLLM) URL() string { return f.srv.URL }

func (f *reflectFakeLLM) Reflects() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.loads
}

// Prompts 返回复盘请求的 user 提示词快照。
func (f *reflectFakeLLM) Prompts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.prompts))
	copy(out, f.prompts)
	return out
}

// TestReflectWritesExperienceE2E 验证复盘真的落进了经验网。
func TestReflectWritesExperienceE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	rsBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/reasoner")
	memBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/memory")

	fakeLLM := startReflectFakeLLM(t)
	boss := startE2EBoss(t)

	scPort := freePort(t)
	rsPort := freePort(t)
	memPort := freePort(t)
	embPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	reasonerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", rsPort)
	memoryURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)

	// 记忆服务带经验网；embedding 后上线，顺带覆盖"种子向量懒补齐"。
	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memoryURL, 60*time.Second)

	bossClient := boss.register(t, centerURL)
	t.Cleanup(func() { _ = bossClient.Close() })

	// 不传 -reflect：要验的就是默认打开。
	startProc(t, rsBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", rsPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
	)
	waitWSReady(t, reasonerURL, 60*time.Second)

	obs, _ := registerObserver(t, centerURL)
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "memory", 60*time.Second)

	startEmbeddingProc(t, centerURL, embPort)
	waitWSReady(t, fmt.Sprintf("ws://127.0.0.1:%d/ws", embPort), 60*time.Second)
	waitRosterHas(t, obs, "embedding", 60*time.Second)

	// 发起一次任务并等它交付完。
	ac, err := protocol.Dial(reasonerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	raw, err := protocol.EncodePayload(map[string]any{"text": "把并发写的序号问题修一下", "conversation_id": "e2e-reflect"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, "e2e", "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "e2e-boss"
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	resp, err := ac.Call(ctx, req)
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
	case text := <-boss.delivered:
		if text != "E2E 复盘链路最终答复" {
			t.Fatalf("交付文本不符: %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("超时：boss 未收到 deliver")
	}

	// 交付完成之后复盘才开始，所以这里必须轮询（不能立刻断言）。
	// 顺手验一件只有真链路能验的事：会话的「入口」是从 boss 灌进 conversations.entry 的
	// （记录是 reasoner 代写的，env.From 永远是 reasoner，不是真正进门的那个人）。
	conversationEntryEventually(t, memoryURL, "e2e-reflect", "e2e-boss", 10*time.Second)

	deadline := time.Now().Add(30 * time.Second)
	var lastHeads protocol.LineHeadsResult
	for time.Now().Before(deadline) {
		rp := memoryCall(t, memoryURL, "lineheads", protocol.LineHeadsPayload{Query: reflectNote, Limit: 8})
		if rp.OK {
			if err := protocol.DecodeRaw(rp.Data, &lastHeads); err != nil {
				t.Fatal(err)
			}
			for _, h := range lastHeads.Heads {
				if strings.Contains(h.Text, "复盘经验") {
					if fakeLLM.Reflects() == 0 {
						t.Fatalf("复盘请求次数为 0，但经验已经出现了——说明这条路不是复盘写的")
					}
					if h.TaskID == "" {
						t.Fatalf("经验应带上来源任务（可回溯）: %+v", h)
					}
					// 流水里必须真的带上这次任务的内容，否则复盘就是在凭空编。
					prompts := fakeLLM.Prompts()
					if len(prompts) == 0 || !strings.Contains(prompts[0], "把并发写的序号问题修一下") {
						t.Fatalf("复盘提示词里应带上用户这次说的话: %q", prompts)
					}
					if !strings.Contains(prompts[0], "[最终答复]") {
						t.Fatalf("复盘提示词里应带上最终答复: %.300s", prompts[0])
					}
					return
				}
			}
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("复盘写回的经验没进网（复盘请求 %d 次）；最后的线头: %+v", fakeLLM.Reflects(), lastHeads.Heads)
}

// conversationEntryEventually 轮询会话清单，直到该会话的入口变成 want。
// "从哪扇门进来"只有真链路能验：记录是 reasoner 代写的，env.From 永远是 reasoner。
func conversationEntryEventually(t *testing.T, memURL, id, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last []protocol.Conversation
	for time.Now().Before(deadline) {
		rp := memoryCall(t, memURL, "conversation_list", protocol.ConversationListPayload{})
		if rp.OK {
			var res protocol.ConversationListResult
			if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
				t.Fatal(err)
			}
			last = res.Conversations
			for _, c := range last {
				if c.ID == id && c.Entry == want {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("会话 %q 的入口在超时前没变成 %q（最后看到: %+v）", id, want, last)
}
