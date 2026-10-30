package tests

// 门户投递端到端（真进程 + 真 WS + 真 SSE）：settings-center + memory + reasoner + frontend（Node）。
//
// 两条链：
//   - TestPortalChainE2E：浏览器 POST /api/message → 门户跳 user_message 给 reasoner
//     （信封 boss = 门户自己）→ 记忆往返（recall → resume）→ 假 LLM → 收尾 deliver 直达门户
//     → SSE `deliver` 事件。boss 丢了的话回答会投给 reasoner 自己（unknown action: deliver）
//     然后无声丢弃——这个测试把它钉住。
//   - TestDeliveryFailureNoticeE2E：boss 指向不在名册的服务 → reasoner 交付失败，
//     按“最后失败了再补一条”的规矩向会话入口（user_message 的信封 from）补一条
//     `[交付失败] …`（同样是 reasoner 主动 Call 来的 deliver）。
//
// frontend 是 Node 服务（无 Go 二进制）：需要先 `cd frontend; npm run build` 产出
// dist/server（前端源码改动后要重建，否则测到的是旧产物）；dist 目录被 .gitignore 忽略。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

// startFrontendProc 用 startProc 启动 frontend 服务（node dist/server/index.js）：
// 脚本路径与 -web 都用仓库内绝对路径——frontend 靠 import.meta.url 定位静态目录与依赖，
// 与进程 cwd 无关，工作目录不会留下任何文件。
func startFrontendProc(t *testing.T, centerURL string, port int) {
	t.Helper()
	root := repoRoot(t)
	script := filepath.Join(root, "frontend", "dist", "server", "index.js")
	if _, err := os.Stat(script); err != nil {
		// 门户是 Node 服务（没有 Go 二进制）：没构建就跳过，而不是红——CI 的 Go 任务不装前端依赖
		t.Skipf("门户全链路 e2e 需要前端构建产物（缺 %s）：先 cd frontend; npm ci; npm run build", script)
	}
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("门户全链路 e2e 需要 Node（PATH 里找不到 node）: %v", err)
	}
	startProc(t, node, script,
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-center", centerURL,
		"-reasoner", "reasoner",
		"-web", filepath.Join(root, "frontend", "dist", "web"),
	)
}

// sseEvent 是一条 SSE 事件（门户的 data 都是 JSON.stringify 的单行 JSON，event 名区分类型）。
type sseEvent struct {
	name string
	data string
}

// openSSE 用标准库打开事件流并后台读进通道。默认 http.Client 不带 Origin、Host 是
// 127.0.0.1:<port>——正好过门户的信任栅栏（Host 必须在回环 / 绑定地址）。
func openSSE(t *testing.T, url string) <-chan sseEvent {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("打开 SSE 失败: %v", err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("打开 SSE 状态码 %d", resp.StatusCode)
	}
	ch := make(chan sseEvent, 64)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		name := ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				select {
				case ch <- sseEvent{name: name, data: strings.TrimSpace(strings.TrimPrefix(line, "data:"))}:
				default: // 缓冲区满也不阻塞读取（本测试只关心少数几条）
				}
			case line == "":
				name = ""
			}
		}
	}()
	return ch
}

// waitSSE 在超时内等一条满足条件的事件；流关闭或超时即失败。
func waitSSE(t *testing.T, ch <-chan sseEvent, timeout time.Duration, what string, ok func(sseEvent) bool) sseEvent {
	t.Helper()
	deadline := time.After(timeout)
	for {
		select {
		case ev, open := <-ch:
			if !open {
				t.Fatalf("SSE 流已关闭（等待 %s）", what)
			}
			if ok(ev) {
				return ev
			}
		case <-deadline:
			t.Fatalf("等待 SSE %s 超时（%s）", what, timeout)
		}
	}
}

// TestPortalChainE2E：门户全链路回程——POST 受理回执 + SSE 收到与假 LLM 固定答复一致的 deliver。
func TestPortalChainE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	alBin := buildService(t, "github.com/NouSart/sump/reasoner")
	memBin := buildService(t, "github.com/NouSart/sump/memory")
	fakeLLM := startFakeDeepSeek(t, false) // 每轮直接回文本，不开工具轮

	scPort := freePort(t)
	alPort := freePort(t)
	memPort := freePort(t)
	fePort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	reasonerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", alPort)
	memoryURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
	feURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", fePort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort),
		"-settings", filepath.Join(t.TempDir(), "settings.json"))
	waitWSReady(t, centerURL, 60*time.Second)

	// memory 先起：reasoner 的记忆往返（recall → resume）要能从名册解析到它。
	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memoryURL, 60*time.Second)

	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
		"-secret-file", filepath.Join(t.TempDir(), "reasoner-secret.json"),
		"-reflect=false", // 复盘会多花一次模型调用；门户用例只验界面链路
	)
	waitWSReady(t, reasonerURL, 60*time.Second)

	// 门户（Node 进程）。
	startFrontendProc(t, centerURL, fePort)
	waitWSReady(t, feURL, 60*time.Second)

	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "memory", 10*time.Second)
	waitRosterHas(t, obs, "frontend", 60*time.Second)

	// 先订阅 SSE 再发消息（deliver 只推给当时的订阅者，不补发）。
	sse := openSSE(t, fmt.Sprintf("http://127.0.0.1:%d/api/events", fePort))

	// 等门户自己的名册视图里出现 reasoner（jumpTo 按它本地名册解析地址）——避免与
	// 门户处理注册回执的时序抢跑。
	waitSSE(t, sse, 60*time.Second, "门户名册含 reasoner", func(ev sseEvent) bool {
		if ev.name != "roster" {
			return false
		}
		var r struct {
			Services []struct {
				Name string `json:"name"`
			} `json:"services"`
		}
		if json.Unmarshal([]byte(ev.data), &r) != nil {
			return false
		}
		for _, s := range r.Services {
			if s.Name == "reasoner" {
				return true
			}
		}
		return false
	})

	// POST /api/message：只应回受理回执（不等 LLM），最终回答从 SSE 回来。
	resp, err := http.Post(
		fmt.Sprintf("http://127.0.0.1:%d/api/message", fePort),
		"application/json",
		strings.NewReader(`{"text":"你好"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var ack struct {
		OK             bool   `json:"ok"`
		ConversationID string `json:"conversationId"`
		Error          string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK {
		t.Fatalf("POST /api/message 未被受理（HTTP %d）: %+v", resp.StatusCode, ack)
	}

	// 断言：SSE 出现 deliver 事件，文本 = 假 LLM 的固定答复。
	const fakeReply = "E2E 最终答复" // 与 startFakeDeepSeek 的固定答复一致
	ev := waitSSE(t, sse, 60*time.Second, "deliver 事件", func(ev sseEvent) bool { return ev.name == "deliver" })
	var dm struct {
		Text           string `json:"text"`
		ConversationID string `json:"conversationId"`
	}
	if err := json.Unmarshal([]byte(ev.data), &dm); err != nil {
		t.Fatal(err)
	}
	if dm.Text != fakeReply {
		t.Fatalf("SSE deliver 文本不符: %q（want %q；若是“[交付失败] …”说明回答没回到门户）", dm.Text, fakeReply)
	}
}

// TestDeliveryFailureNoticeE2E：boss 认错（不在名册）时，补报必须能看见——
// 入口方（信封 from）在自己的 WS 端点上收到一条以 `[交付失败]` 开头的 deliver。
func TestDeliveryFailureNoticeE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	alBin := buildService(t, "github.com/NouSart/sump/reasoner")
	memBin := buildService(t, "github.com/NouSart/sump/memory")
	fakeLLM := startFakeDeepSeek(t, false)

	scPort := freePort(t)
	alPort := freePort(t)
	memPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	reasonerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", alPort)
	memoryURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort),
		"-settings", filepath.Join(t.TempDir(), "settings.json"))
	waitWSReady(t, centerURL, 60*time.Second)

	// 入口替身：复用 e2eBoss（infer_e2e_test.go）——它注册进真中心、在 WS 端点上实现
	// deliver 并把文本记进 chan，正好扮演“能收到补报的会话入口”。先注册，保证 reasoner
	// 启动时的名册里就有它（交付失败时它要能被解析到）。
	const entryName = "e2e-boss" // 与 startE2EBoss 的注册名一致
	entry := startE2EBoss(t)
	entryReg := entry.register(t, centerURL)
	t.Cleanup(func() { _ = entryReg.Close() })

	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memoryURL, 60*time.Second)

	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
		"-secret-file", filepath.Join(t.TempDir(), "reasoner-secret.json"),
		"-reflect=false", // 复盘会多花一次模型调用；门户用例只验界面链路
	)
	waitWSReady(t, reasonerURL, 60*time.Second)

	obs, err := protocol.Dial(centerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "memory", 10*time.Second)
	waitRosterHas(t, obs, entryName, 10*time.Second)

	// 从入口替身发 user_message：boss 故意指向不在名册的服务 → 交付必然失败。
	ac, err := protocol.Dial(reasonerURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ac.Close() })
	raw, err := protocol.EncodePayload(map[string]any{"text": "你好", "conversation_id": "e2e-portal-fail-conv"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := protocol.NewEnvelope(protocol.TypeJump, entryName, "reasoner", "", protocol.JumpPayload{Action: "user_message", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	req.Boss = "no-such-boss"
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

	// 断言：入口在自己的 WS 端点上收到补报（reasoner 主动同步 Call 来的），前缀 `[交付失败]`。
	select {
	case text := <-entry.delivered:
		if !strings.HasPrefix(text, "[交付失败]") {
			t.Fatalf("补报文本应以 [交付失败] 开头: %q", text)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("超时：入口未收到 [交付失败] 补报")
	}
}
