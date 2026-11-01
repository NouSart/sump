package tests

// 群聊 e2e：真进程全链——stub NapCat 发群消息 → qq（标来源：群名 / 人数 / 谁说的 / 是否@我）
// → reasoner（假 LLM 判 done 并交付）→ memory（落库带标签与结构化 meta）→ deliver 回发**群**。
//
// 为什么单独一条：私聊那条链（qq_e2e / file_e2e）证明不了"多人入口"的关键事实——
// 标签写没写对、扩散面（人数）有没有、答复发回的是群还是私聊。这些错一个，模型就会在群里
// 把陌生人的话当指令，或者把该公开的答复发错地方。

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
)

// groupFakeLLM：第一轮判 done（群里这条消息不需要追问），之后给最终答复。
type groupFakeLLM struct {
	mu    sync.Mutex
	calls int
	srv   *httptest.Server
}

func startGroupFakeLLM(t *testing.T) *groupFakeLLM {
	t.Helper()
	f := &groupFakeLLM{}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		_, _ = io.Copy(io.Discard, r.Body)
		f.mu.Lock()
		f.calls++
		call := f.calls
		f.mu.Unlock()

		var msg map[string]any
		if call == 1 {
			msg = map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{
					"id":   "call_status",
					"type": "function",
					"function": map[string]any{
						"name":      "task_status",
						"arguments": `{"status":"done","title":"群里问的会"}`,
					},
				}},
			}
		} else {
			msg = map[string]any{"role": "assistant", "content": "群里收到的答复"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *groupFakeLLM) URL() string { return f.srv.URL }

// qqGroupEvent 构造一条群消息（OneBot 11 normal group message）。
func qqGroupEvent(groupID, uid int64, text string, mentionSelf bool) map[string]any {
	segs := []any{}
	if mentionSelf {
		segs = append(segs, map[string]any{"type": "at", "data": map[string]any{"qq": "2578428650"}})
	}
	segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
	return map[string]any{
		"post_type": "message", "message_type": "group", "sub_type": "normal",
		"group_id": groupID, "user_id": uid, "self_id": 2578428650,
		"message_id": 7, "time": time.Now().Unix(), "raw_message": text, "font": 0,
		"sender":  map[string]any{"user_id": uid, "nickname": "张三", "card": "张三", "role": "member"},
		"message": segs,
	}
}

// jumpToService 以「测试客户端」身份调一个服务的动作（走真 WS，不绕过协议）。
func jumpToService(t *testing.T, addr, to, action string, input any) map[string]any {
	t.Helper()
	c, err := protocol.Dial(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "e2e", to, "", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	if !rp.OK {
		t.Fatalf("%s.%s 失败: %s", to, action, rp.Error)
	}
	var out map[string]any
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// TestQQGroupE2E 群消息进上下文、来源标注带全、答复回到群里，且记忆里存的是同一句话。
func TestQQGroupE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	memBin := buildService(t, "github.com/NouSart/sump/memory")
	alBin := buildService(t, "github.com/NouSart/sump/reasoner")
	qqBin := buildService(t, "github.com/NouSart/sump/qq")
	nc := startNapCatStub(t)
	fakeLLM := startGroupFakeLLM(t)

	scPort := freePort(t)
	memPort := freePort(t)
	alPort := freePort(t)
	qqPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	memURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
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
	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
		"-reflect=false",
	)
	waitWSReady(t, alURL, 60*time.Second)
	startProc(t, qqBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", qqPort),
		"-center", centerURL,
		"-napcat", nc.URL(),
		"-owner", "2271917353",
	)
	waitWSReady(t, qqURL, 60*time.Second)

	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	for _, name := range []string{"memory", "reasoner", "qq"} {
		waitRosterHas(t, obs, name, 60*time.Second)
	}

	// 群里非主人发言并 @ 了它。
	nc.waitConn(t, 60*time.Second)
	nc.sendEvent(t, qqGroupEvent(67890, 12345, "今天几点开会", true))

	// 1) 答复发回**群**（不是私聊、不是发错群）。
	act := nc.waitSendMsg(t, 60*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message_type"] != "group" {
		t.Fatalf("答复应发回群聊: %+v", params)
	}
	if gid, _ := params["group_id"].(float64); int64(gid) != 67890 {
		t.Fatalf("答复群号不符: %+v", params)
	}
	if params["message"] != "群里收到的答复" {
		t.Fatalf("答复文本不符: %+v", params)
	}

	// 2) 记忆里存的是同一句话：正文带来源标注（群名 / 人数 / 谁 / 非主人 / 已@我）。
	hist := jumpToService(t, memURL, "memory", "history", map[string]any{
		"conversation_id": "qq:group:67890",
		"all_tasks":       true,
		"limit":           50,
	})
	rawMsgs, _ := hist["messages"].([]any)
	if len(rawMsgs) == 0 {
		t.Fatal("记忆里没有这个群会话的记录")
	}
	var userLine map[string]any
	for i := len(rawMsgs) - 1; i >= 0; i-- {
		m, _ := rawMsgs[i].(map[string]any)
		if m["role"] == "user" {
			userLine = m
			break
		}
	}
	if userLine == nil {
		t.Fatal("没有 user 记录")
	}
	content, _ := userLine["content"].(string)
	for _, want := range []string{"[群聊 · ", "群「编程交流」(67890)", "12 人可见", "张三(12345)", "非主人", "已@我", "今天几点开会"} {
		if !strings.Contains(content, want) {
			t.Fatalf("落库正文缺 %q: %q", want, content)
		}
	}
	// 结构化来源跟着记录走（门户/程序读的是它，不是正文标签）。
	metaRaw, _ := userLine["meta"].(string)
	var meta struct {
		Sender map[string]any `json:"sender"`
	}
	if err := json.Unmarshal([]byte(metaRaw), &meta); err != nil || meta.Sender == nil {
		t.Fatalf("记录 meta 应带结构化 sender: %q", metaRaw)
	}
	if meta.Sender["group_name"] != "编程交流" || meta.Sender["id"] != "12345" {
		t.Fatalf("meta.sender 不符: %+v", meta.Sender)
	}

	// 3) 任务名是人话，不是那串标签（标签会显示在历史列表里，没人想点开一坨方括号）。
	// 模型这次顺带把任务名改成了更像人写的标题——两条路径（入口给的正文名 / 模型改的名）
	// 都要求"不含标签"。
	tasks := jumpToService(t, memURL, "memory", "task_list", map[string]any{"conversation_id": "qq:group:67890"})
	rawTasks, _ := tasks["tasks"].([]any)
	if len(rawTasks) == 0 {
		t.Fatal("群会话没有任务")
	}
	first, _ := rawTasks[0].(map[string]any)
	name, _ := first["name"].(string)
	if name == "" || strings.Contains(name, "群聊") || strings.Contains(name, "[") {
		t.Fatalf("任务名不该是来源标签: %q", name)
	}
	if name != "群里问的会" {
		t.Fatalf("任务名应取模型判定时给的标题: %q", name)
	}
}
