// 语义召回的链路测试：embedding 服务替身（可控向量）——
// remember 时向量化、recall 语义排序与阈值、懒补齐（算一次落库复用）、embedding 缺席时静默回落 bigram。
package tests

import (
	"context"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// ---------- embedding 服务替身 ----------

type stubEmbedding struct {
	srv  *spv2.Server
	http *httptest.Server

	mu       sync.Mutex
	requests []protocol.EmbedPayload
	vectors  map[string][]float64 // 文本 → 向量（未配置的文本回零向量）
	delay    time.Duration        // 回复前先睡这么久（模拟慢/挂住的上游）
	fail     bool                 // 回复一个错误（模拟上游报错）
}

// SetDelay 让替身每次回复前先睡 d（模拟"上游很慢甚至挂住"）。
func (e *stubEmbedding) SetDelay(d time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.delay = d
}

// SetFail 让替身回错误（模拟上游中途坏掉）。
func (e *stubEmbedding) SetFail(f bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.fail = f
}

func startStubEmbedding(t *testing.T, center *stubCenter) *stubEmbedding {
	return startStubEmbeddingNamed(t, center, "embedding")
}

// startStubEmbeddingNamed 同上，但可指定注册名（测 context.embedding 热改用）。
func startStubEmbeddingNamed(t *testing.T, center *stubCenter, name string) *stubEmbedding {
	t.Helper()
	e := &stubEmbedding{vectors: map[string][]float64{}}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: name, Actor: spv2.ServiceRef(name),
		Version: "stub/1", Methods: []spv2.Method{{Name: "embed", Shape: spv2.ShapeCall}},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	e.srv = srv
	srv.Handle("embed", e.handleEmbed)
	e.http = httptest.NewServer(srv.Handler())
	t.Cleanup(e.http.Close)

	// 注册进中心（走真实注册路径：名册广播后 memory 才能解析到 embedding）。
	registerService(t, center.URL(), name, e.URL())
	return e
}

func (e *stubEmbedding) URL() string {
	return "ws" + strings.TrimPrefix(e.http.URL, "http") + "/spv2"
}

func (e *stubEmbedding) SetVectors(m map[string][]float64) {
	e.mu.Lock()
	defer e.mu.Unlock()
	for k, v := range m {
		e.vectors[k] = v
	}
}

// Requests 返回收到的 embed 请求快照。
func (e *stubEmbedding) Requests() []protocol.EmbedPayload {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]protocol.EmbedPayload(nil), e.requests...)
}

// RequestsFor 统计包含指定文本的请求条数。
func (e *stubEmbedding) RequestsFor(text string) int {
	n := 0
	for _, r := range e.Requests() {
		for _, t := range r.Texts {
			if t == text {
				n++
				break
			}
		}
	}
	return n
}

func (e *stubEmbedding) handleEmbed(_ context.Context, call *spv2.Call) error {
	var p protocol.EmbedPayload
	if err := call.Frame.DecodeBody(&p); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, "bad payload", false, nil)
	}
	e.mu.Lock()
	e.requests = append(e.requests, p)
	delay, fail := e.delay, e.fail
	vecs := make([][]float64, 0, len(p.Texts))
	for _, text := range p.Texts {
		v := e.vectors[text]
		if v == nil {
			v = []float64{0, 0} // 未配置 → 零向量（低于阈值，便于断言"不该命中"）
		}
		vecs = append(vecs, v)
	}
	e.mu.Unlock()
	if delay > 0 {
		time.Sleep(delay)
	}
	if fail {
		return call.ReplyError(spv2.CodeUnavailable, "stub: 上游挂了", false, nil)
	}
	return call.Reply(protocol.EmbedResult{Vectors: vecs, Dim: 2, Model: "stub-1"})
}

// ---------- 辅助 ----------

// waitRosterName 等待 memory 的名册里出现某服务（注册广播是异步的）。
func waitRosterName(t *testing.T, srv *server.Server, name string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		for _, c := range srv.Roster() {
			if c.Name == name {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("名册中未见 %s", name)
}

// remember 向 memory 写一条长期记忆。
func remember(t *testing.T, caller *stubCaller, srv *server.Server, kind, content string) {
	t.Helper()
	if rp := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: kind, Content: content}); !rp.OK {
		t.Fatalf("remember 失败: %s", rp.Error)
	}
}

// ---------- 用例 ----------

// TestSemanticRecallRanking：写入时向量化；recall 用语义排序（高相似命中、低相似被阈值滤掉）；
// 查询走 Query=true、写入走 Query=false 的请求形态。
func TestSemanticRecallRanking(t *testing.T) {
	sc := startStubCenter(t)
	emb := startStubEmbedding(t, sc)
	emb.SetVectors(map[string][]float64{
		"苹果":   {1, 0},
		"爱吃苹果": {0.9, 0.1}, // cos ≈ 0.994 → 命中
		"今天天气": {0.1, 0.9}, // cos ≈ 0.110 → 低于阈值被滤掉
	})
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())
	waitRosterName(t, srv, "embedding")

	remember(t, caller, srv, "偏好", "爱吃苹果")
	remember(t, caller, srv, "知识", "今天天气")

	block := recallBlock(t, caller, srv, "semantic-1")
	if !strings.Contains(block, "爱吃苹果") {
		t.Fatalf("语义召回应命中「爱吃苹果」: %q", block)
	}
	if strings.Contains(block, "今天天气") {
		t.Fatalf("低相似条目应被阈值滤掉: %q", block)
	}

	// 请求形态：写入 Query=false；查询 Query=true。
	var sawWrite, sawQuery bool
	for _, r := range emb.Requests() {
		if containsText(r.Texts, "爱吃苹果") && !r.Query {
			sawWrite = true
		}
		if containsText(r.Texts, "苹果") && r.Query {
			sawQuery = true
		}
	}
	if !sawWrite || !sawQuery {
		t.Fatalf("embed 请求形态不符: write=%v query=%v", sawWrite, sawQuery)
	}
}

// TestSemanticBackfillPersists：embedding 后上线——首次 recall 懒补齐（含条目文本的批量请求）
// 并落库；第二次 recall 不再请求条目文本（向量已复用）。
func TestSemanticBackfillPersists(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12) // 先没有 embedding
	caller := startStubCaller(t, sc.URL())

	remember(t, caller, srv, "偏好", "爱吃苹果")
	remember(t, caller, srv, "知识", "今天天气")

	emb := startStubEmbedding(t, sc) // 之后才上线
	emb.SetVectors(map[string][]float64{
		"苹果":   {1, 0},
		"爱吃苹果": {0.9, 0.1},
		"今天天气": {0.1, 0.9},
	})
	waitRosterName(t, srv, "embedding")

	block := recallBlock(t, caller, srv, "backfill-1")
	if !strings.Contains(block, "爱吃苹果") || strings.Contains(block, "今天天气") {
		t.Fatalf("懒补齐后语义召回不符: %q", block)
	}
	if n := emb.RequestsFor("爱吃苹果"); n != 1 {
		t.Fatalf("首次召回应补齐「爱吃苹果」向量（1 次），实际 %d", n)
	}

	block = recallBlock(t, caller, srv, "backfill-2")
	if !strings.Contains(block, "爱吃苹果") {
		t.Fatalf("第二次召回仍应命中: %q", block)
	}
	if n := emb.RequestsFor("爱吃苹果"); n != 1 {
		t.Fatalf("向量已落库——第二次召回不应再请求条目文本，实际 %d 次", n)
	}
}

// TestSemanticFallbackBigram：embedding 缺席（名册无此服务）→ 静默回落 bigram（子串命中仍有效）。
func TestSemanticFallbackBigram(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	remember(t, caller, srv, "偏好", "爱吃苹果")

	block := recallBlock(t, caller, srv, "fallback-1")
	if !strings.Contains(block, "爱吃苹果") {
		t.Fatalf("bigram 兜底应命中: %q", block)
	}
}

func containsText(texts []string, want string) bool {
	for _, t := range texts {
		if t == want {
			return true
		}
	}
	return false
}
