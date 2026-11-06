package tests

// M8 总测试：跨语言真进程——settings-center + memory（Go）+ embedding（Python，fake 后端）。
//
// 场景：memory 先起（名册无 embedding）→ Python embedding 后上线（热注册）→ remember 三条
// → 首次 recall 经 Python 服务懒补齐向量并语义排序 → resume 回观察者。
//
// 判别设计：查询 "apple" 时——
//   - "apple pie" 在 embedding 几何（词项集合）下重叠 → 必须命中；
//   - "apples are red" 是 bigram 子串命中、但词项集合不重叠 → **若走了 bigram 兜底它就会出现**，
//     所以"它不在"即证明本次召回走的是 embedding 路径（跨语言链路真的通了）。
//   - "天气好" 两边都不命中 → 不应出现。

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// pythonExe 选择运行 embedding 服务的解释器：仓库内 venv 优先（本地开发），
// 其次环境变量 SUMP_PYTHON，最后 PATH 上的 python（CI 用系统解释器 + pip 安装的依赖）。
func pythonExe(t *testing.T) string {
	t.Helper()
	root := repoRoot(t)
	for _, c := range []string{
		filepath.Join(root, "src", "embedding", ".venv", "Scripts", "python.exe"),
		filepath.Join(root, "src", "embedding", ".venv", "bin", "python"),
	} {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			return c
		}
	}
	if env := os.Getenv("SUMP_PYTHON"); env != "" {
		return env
	}
	return "python"
}

// startEmbeddingProc 启动 Python embedding 服务（fake 后端——无模型依赖、确定性）。
func startEmbeddingProc(t *testing.T, centerURL string, port int) {
	t.Helper()
	cmd := exec.Command(pythonExe(t), "-m", "sump_embedding",
		"-addr", fmt.Sprintf("127.0.0.1:%d", port),
		"-center", centerURL,
		"-backend", "fake",
		"-heartbeat-ms", "1000",
	)
	cmd.Dir = filepath.Join(repoRoot(t), "src", "embedding")
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动 embedding 服务失败（%s）: %v（需要 Python ≥3.12 且已装 embedding 依赖，见 CONTRIBUTING.md）", cmd.Path, err)
	}
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
}

// ---------- resume 观察者（测试进程内的“调用方”替身） ----------

type resumeProbe struct {
	srv     *httptest.Server
	resumes chan protocol.ResumePayload
}

func startResumeProbe(t *testing.T, centerURL string) *resumeProbe {
	t.Helper()
	p := &resumeProbe{resumes: make(chan protocol.ResumePayload, 16)}
	srv := newStubServer(t, "probe", "e2e resume 观察者", spv2.Method{Name: "resume", Shape: spv2.ShapeCall})
	srv.Handle("resume", func(_ context.Context, call *spv2.Call) error {
		var p2 protocol.ResumePayload
		if err := call.Frame.DecodeBody(&p2); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		select {
		case p.resumes <- p2:
		default:
		}
		return call.Reply(map[string]string{"status": "accepted"})
	})
	p.srv = serveStub(t, srv)
	// 注册进中心：memory 依赖名册解析 resume 目标（probe 的名字必须与 recall 的 from 一致）。
	registerService(t, centerURL, "probe", p.URL(), "e2e resume 观察者")
	return p
}

func (p *resumeProbe) URL() string { return wsURL(p.srv) }

// ---------- 用例 ----------

// TestSemanticRecallCrossLanguageE2E：Go ↔ Python 跨语言语义召回全链（含热注册与懒补齐）。
func TestSemanticRecallCrossLanguageE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	memBin := buildService(t, "github.com/NouSart/sump/memory")

	dir := t.TempDir()
	scPort := freePort(t)
	centerURL := spv2URL(scPort)
	memPort := freePort(t)
	memURL := spv2URL(memPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort), "-settings", filepath.Join(dir, "settings.json"))
	waitWSReady(t, centerURL, 60*time.Second)

	// memory 先起：此刻名册没有 embedding——remember 的向量化会失败（懒补齐交给之后的 recall）。
	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(dir, "memory.db"))
	waitWSReady(t, memURL, 60*time.Second)

	obs, _ := registerObserver(t, centerURL)
	waitRosterHas(t, obs, "memory", 60*time.Second)
	probe := startResumeProbe(t, centerURL)

	// 写入三条（embedding 尚未上线 → 向量留空）。
	for _, content := range []string{"apple pie", "apples are red", "天气好"} {
		rp := memoryCall(t, memURL, "remember", protocol.RememberPayload{Kind: "偏好", Content: content})
		if !rp.OK {
			t.Fatalf("remember %q 失败: %s", content, rp.Error)
		}
	}

	// Python embedding 后上线：热注册 → 中心广播名册（memory 随之可解析）。
	embPort := freePort(t)
	startEmbeddingProc(t, centerURL, embPort)
	waitWSReady(t, spv2URL(embPort), 60*time.Second)
	waitRosterHas(t, obs, "embedding", 60*time.Second)

	block := recallStable(t, probe, memURL, "apple", "e2e-semantic-1")
	if !strings.Contains(block, "apple pie") {
		t.Fatalf("语义召回应命中「apple pie」: %q", block)
	}
	if strings.Contains(block, "apples are red") {
		t.Fatalf("出现「apples are red」= 走了 bigram 兜底（embedding 几何不命中它）: %q", block)
	}
	if strings.Contains(block, "天气好") {
		t.Fatalf("无关条目不应出现: %q", block)
	}
}

// memoryCall 直连 memory 发一次原生调用并等回执（短连接）。
func memoryCall(t *testing.T, memURL, action string, input any) callResult {
	t.Helper()
	c := dialSPV2(t, memURL)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return callJumpFrom(t, c, "e2e", "memory", action, input, ctx)
}

// recallStable 发起 recall 并返回记忆块；名册广播存在毫秒级异步窗口——
// 若结果仍含 bigram 兜底特征（"apples are red"）则短暂重试，直到语义路径稳定或超时。
func recallStable(t *testing.T, p *resumeProbe, memURL, query, conv string) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var lastBlock, lastErr string
	for time.Now().Before(deadline) {
		block, err := recallOnce(t, p, memURL, query, conv)
		if err != nil {
			lastErr = err.Error()
			time.Sleep(200 * time.Millisecond)
			continue
		}
		lastBlock = block
		if block != "" && !strings.Contains(block, "apples are red") {
			return block
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待语义召回稳定失败；最后记忆块: %q；最后错误: %s", lastBlock, lastErr)
	return ""
}

// recallOnce 以 probe 的身份发起一次 recall，等待 resume 并提取记忆块（【记忆】开头的 system 消息）。
func recallOnce(t *testing.T, p *resumeProbe, memURL, query, conv string) (string, error) {
	t.Helper()
	c := dialSPV2(t, memURL)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rp := callJumpFrom(t, c, "probe", "memory", "recall", protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: conv,
		Boss:           "probe",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: query},
		},
	}}, ctx)
	if !rp.OK {
		return "", fmt.Errorf("recall 未被受理: %s", rp.Error)
	}
	select {
	case r := <-p.resumes:
		for _, m := range r.Context.Messages {
			if m.Role == "system" && strings.HasPrefix(m.Content, "【记忆】") {
				return m.Content, nil
			}
		}
		return "", nil
	case <-time.After(20 * time.Second):
		return "", fmt.Errorf("超时：未收到 resume")
	}
}
