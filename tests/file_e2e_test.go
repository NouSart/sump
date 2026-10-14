package tests

// M7 e2e：真进程全链——stub NapCat 发文件 → qq → files（真入库）→ reasoner → memory
// → 假 LLM（从占位文本提取 id、调 files__read 自取文本）→ deliver 回发 QQ。
// 验证点：占位带 id / read 结果注入模型 / 链终点回发。

import (
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

// ---------- 文件链专用假 LLM ----------

// fileFakeLLM：第 1 轮从用户消息的占位文本（[文件: 名, id=xxx]）提取 id 并调
// files__read；其后轮次直接回最终答复（[工具结果] 由通知模式唤醒注入）。
type fileFakeLLM struct {
	mu       sync.Mutex
	calls    int
	requests []map[string]any
	readID   string
	srv      *httptest.Server
}

func startFileFakeLLM(t *testing.T) *fileFakeLLM {
	t.Helper()
	f := &fileFakeLLM{}
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
		if call == 1 {
			id := extractFileID(req)
			f.mu.Lock()
			f.readID = id
			f.mu.Unlock()
			if id != "" {
				msg = map[string]any{
					"role": "assistant",
					"tool_calls": []any{map[string]any{
						"id":   "call_read",
						"type": "function",
						"function": map[string]any{
							"name":      "files__read",
							"arguments": fmt.Sprintf(`{"id":%q}`, id),
						},
					}},
				}
			}
		}
		if msg == nil {
			msg = map[string]any{"role": "assistant", "content": "E2E 最终答复"}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": msg}}})
	}))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fileFakeLLM) URL() string { return f.srv.URL }

func (f *fileFakeLLM) ReadID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.readID
}

func (f *fileFakeLLM) Requests() []map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]any, len(f.requests))
	copy(out, f.requests)
	return out
}

// extractFileID 从请求消息里提取占位文本携带的文件 id（"[文件: 名, id=xxx]"）。
func extractFileID(req map[string]any) string {
	msgs, _ := req["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		c, _ := m["content"].(string)
		if j := strings.Index(c, "id="); j >= 0 {
			rest := c[j+3:]
			if k := strings.IndexByte(rest, ']'); k > 0 {
				return rest[:k]
			}
		}
	}
	return ""
}

// qqPrivateFileEvent 构造一条带文件附件的私聊事件（OneBot 11 file 段）。
func qqPrivateFileEvent(uid int64, text, fileURL, fileName string) map[string]any {
	segs := []any{}
	if text != "" {
		segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
	}
	segs = append(segs, map[string]any{"type": "file", "data": map[string]any{"url": fileURL, "file": fileName}})
	return map[string]any{
		"post_type": "message", "message_type": "private", "sub_type": "friend",
		"user_id": uid, "self_id": 2578428650, "message_id": 2, "time": time.Now().Unix(),
		"raw_message": text, "font": 0, "message": segs,
	}
}

// ---------- 用例 ----------

// TestQQFileE2E 是 M7 的验收：主人私聊发文件 → files 真入库 → 假 LLM 凭占位 id
// 调 files__read 自取文本（结果注入下一轮）→ 最终答复回发 QQ。
func TestQQFileE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	memBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/memory")
	filBin := buildServiceNamed(t, "github.com/QAQ-awa-QAQ/sump/files", "sump-files")
	alBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/reasoner")
	qqBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/qq")
	nc := startNapCatStub(t)
	fakeLLM := startFileFakeLLM(t)

	// 素材：httptest 提供一个 Markdown 文件（files 服务会来下载）。
	md := []byte("# 周报\n会议要点：季度营收上涨\n")
	mdSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/markdown")
		_, _ = w.Write(md)
	}))
	t.Cleanup(mdSrv.Close)

	scPort := freePort(t)
	memPort := freePort(t)
	filPort := freePort(t)
	alPort := freePort(t)
	qqPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	memURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
	filURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", filPort)
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

	startProc(t, filBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", filPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "files.db"),
		"-dir", t.TempDir(),
	)
	waitWSReady(t, filURL, 60*time.Second)

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
	for _, name := range []string{"memory", "files", "reasoner", "qq"} {
		waitRosterHas(t, obs, name, 60*time.Second)
	}

	// 主人私聊发文件 + 文本。
	nc.waitConn(t, 60*time.Second)
	nc.sendEvent(t, qqPrivateFileEvent(2271917353, "看看这个文件", mdSrv.URL+"/report.md", "report.md"))

	// 最终回复经 send_msg 发回。
	act := nc.waitSendMsg(t, 60*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message"] != "E2E 最终答复" {
		t.Fatalf("回复文本不符: %+v", params)
	}
	if int64(params["user_id"].(float64)) != 2271917353 || params["message_type"] != "private" {
		t.Fatalf("回复目标不符: %+v", params)
	}

	// 假 LLM 第一轮应从占位文本提取到文件 id（占位带 id 的证明）。
	if fakeLLM.ReadID() == "" {
		t.Fatal("占位文本未携带文件 id（假 LLM 未能提取）")
	}

	// 工具结果应注入后续某一轮请求（受理轮与结果轮不必相邻——中间可能有回执轮），
	// 扫描全部请求而非固定下标。
	reqs := fakeLLM.Requests()
	if len(reqs) < 2 {
		t.Fatalf("假 LLM 调用轮次 = %d, want >= 2", len(reqs))
	}
	foundToolResult := false
	foundContent := false
	for _, r := range reqs {
		b, _ := json.Marshal(r)
		if strings.Contains(string(b), "[工具结果] files__read") {
			foundToolResult = true
		}
		if strings.Contains(string(b), "季度营收上涨") {
			foundContent = true
		}
	}
	if !foundToolResult {
		t.Fatalf("缺少 files__read 工具结果注入: %+v", reqs[len(reqs)-1])
	}
	if !foundContent {
		t.Fatalf("文件内容未注入模型: %+v", reqs[len(reqs)-1])
	}
}
