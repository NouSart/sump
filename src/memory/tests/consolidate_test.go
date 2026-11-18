package tests

// 记忆整理端到端：用户消息入池 → consolidate 调 reasoner.think → 写入长期记忆。
// 全程 SPv2.1 原生调用；模型失败时候选留在池里重试。

import (
	"context"
	"fmt"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/experience/longterm"
	"github.com/NouSart/sump/memory/experience/shared"
	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// stubReasoner 是只实现 think 的 reasoner 替身：回复内容可切换。
type stubReasoner struct {
	srv  *spv2.Server
	http *httptest.Server

	mu   sync.Mutex
	text string
}

func startStubReasoner(t *testing.T, centerURL string) *stubReasoner {
	t.Helper()
	r := &stubReasoner{}
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "reasoner", Actor: spv2.ServiceRef("reasoner"),
		Version: "stub/1", Methods: []spv2.Method{
			{Name: "think", Shape: spv2.ShapeCall},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	r.srv = srv
	srv.Handle("think", func(_ context.Context, call *spv2.Call) error {
		r.mu.Lock()
		text := r.text
		r.mu.Unlock()
		return call.Reply(protocol.ThinkResult{Text: text})
	})
	r.http = httptest.NewServer(srv.Handler())
	t.Cleanup(r.http.Close)
	registerService(t, centerURL, "reasoner", r.URL())
	return r
}

func (r *stubReasoner) URL() string {
	return "ws" + strings.TrimPrefix(r.http.URL, "http") + "/spv2"
}

func (r *stubReasoner) setText(text string) {
	r.mu.Lock()
	r.text = text
	r.mu.Unlock()
}

// callMemory 向 memory 发一次原生调用并等回执（短连接）。
func callMemory(t *testing.T, memoryURL, method string, in, out any) error {
	t.Helper()
	return callMemoryFrom(t, memoryURL, "tester", method, in, out)
}

func callMemoryFrom(t *testing.T, memoryURL, from, method string, in, out any) error {
	t.Helper()
	return callWithWork(t, memoryURL, from, "w-consolidate", method, in, out)
}

func callWithWork(t *testing.T, memoryURL, from, work, method string, in, out any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := spv2.Dial(ctx, memoryURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	call, err := spv2.NewCall(work, spv2.ServiceRef(from), spv2.ServiceRef("memory"), method, in)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := conn.Call(ctx, call)
	if err != nil {
		return err
	}
	if out != nil {
		return resp.DecodeBody(out)
	}
	return nil
}

// consolidateUntilReady 处理「reasoner 刚注册、memory 名册未更新」的时序。
func consolidateUntilReady(t *testing.T, memoryURL string) (protocol.ConsolidateResult, error) {
	t.Helper()
	for i := 0; i < 30; i++ {
		var res protocol.ConsolidateResult
		err := callMemory(t, memoryURL, "consolidate", struct{}{}, &res)
		if err == nil {
			return res, nil
		}
		if !strings.Contains(err.Error(), "未知目标服务") {
			return protocol.ConsolidateResult{}, err
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("consolidate 重试超时：memory 名册中始终未见 reasoner")
	return protocol.ConsolidateResult{}, nil
}

func findEntry(entries []longterm.Entry, content string) (longterm.Entry, bool) {
	for _, e := range entries {
		if e.Content == content {
			return e, true
		}
	}
	return longterm.Entry{}, false
}

// pendingID 取当前唯一的待整理候选 id（测试里铺一条就取一条）。
func pendingID(t *testing.T, st *store.Store) int64 {
	t.Helper()
	items, err := st.PendingStaged(0)
	if err != nil || len(items) != 1 {
		t.Fatalf("应有 1 条待整理候选: %+v err=%v", items, err)
	}
	return items[0].ID
}

func TestConsolidateArchivesStagedCandidates(t *testing.T) {
	sc := startStubCenter(t)
	var st *store.Store
	var sh *shared.Store
	srv := startMemory(t, sc.URL(), 12, func(c *stackConfig) {
		st, sh = c.Store, c.Shared
	})
	stub := startStubReasoner(t, sc.URL())
	srv.addPeer("reasoner", stub.URL())
	if _, addr, err := srv.exp.Resolve("reasoner"); err != nil || addr != stub.URL() {
		t.Fatalf("reasoner peer 注入失败: addr=%q err=%v want=%q", addr, err, stub.URL())
	}

	// remember 的 salience 参数要校验；合法值只升不降。
	var bad protocol.RememberResult
	if err := callMemory(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "测试记忆", Salience: 1.5}, &bad); err == nil {
		t.Fatal("salience 越界应报错")
	}
	var first protocol.RememberResult
	if err := callMemory(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "会合并的记忆", Salience: 0.2}, &first); err != nil || !first.Created {
		t.Fatalf("remember 失败: %+v err=%v", first, err)
	}
	var merged protocol.RememberResult
	if err := callMemory(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "身份", Content: "会合并的记忆", Salience: 0.9}, &merged); err != nil || merged.Created {
		t.Fatalf("重复 remember 应幂等合并: %+v err=%v", merged, err)
	}
	entries, err := sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := findEntry(entries, "会合并的记忆"); !ok || e.Salience != 0.9 {
		t.Fatalf("salience 应只升不降: %+v", e)
	}

	// 用户消息进入整理池；助手答复不进。
	if err := callMemory(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "c1", Role: "user", Content: "我住在杭州"}, nil); err != nil {
		t.Fatal(err)
	}
	if err := callMemory(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "c1", Role: "assistant", Content: "好的"}, nil); err != nil {
		t.Fatal(err)
	}
	if pending, _, _, _, err := st.StagingCounts(); err != nil || pending != 1 {
		t.Fatalf("整理池应只有 1 条用户候选: pending=%d err=%v", pending, err)
	}

	pid := pendingID(t, st)
	stub.setText("结果：\n```json\n" + fmt.Sprintf(`[{"id":%d,"keep":true,"kind":"身份","content":"住在杭州","salience":0.7,"priority":0,"durability":"durable","reason":"稳定事实"}]`, pid) + "\n```")
	res, err := consolidateUntilReady(t, srv.WsURL())
	if err != nil {
		t.Fatalf("consolidate 失败: %v", err)
	}
	if res.Processed != 1 || res.Archived != 1 || res.Failed != 0 {
		t.Fatalf("整理结果不符: %+v", res)
	}
	entries, err = sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := findEntry(entries, "住在杭州"); !ok || e.Salience != 0.7 {
		t.Fatalf("整理后的长期记忆不符: %+v", e)
	}
	if pending, done, _, _, err := st.StagingCounts(); err != nil || pending != 0 || done != 1 {
		t.Fatalf("整理池计数不符: pending=%d done=%d err=%v", pending, done, err)
	}

	// 冲突 keep_old：模型选择保留旧条目 → 新候选 skipped，旧条目不动。
	if err := callMemory(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "c1", Role: "user", Content: "我养了一只猫"}, nil); err != nil {
		t.Fatal(err)
	}
	pid = pendingID(t, st)
	stub.setText(fmt.Sprintf(`[{"id":%d,"keep":true,"kind":"身份","content":"养猫","salience":0.5,"priority":0,"durability":"durable","conflicts":[{"id":%d,"resolution":"keep_old"}]}]`, pid, merged.ID))
	res, err = consolidateUntilReady(t, srv.WsURL())
	if err != nil {
		t.Fatalf("keep_old 冲突不应报错: %v", err)
	}
	if res.Skipped != 1 || res.Conflicts != 1 {
		t.Fatalf("keep_old 应跳过候选: %+v", res)
	}
	if entries, err = sh.Longterm.ListActive(); err != nil {
		t.Fatal(err)
	}
	if _, ok := findEntry(entries, "养猫"); ok {
		t.Fatal("keep_old 不应写入新条目")
	}

	// transient：只属于当前任务，不入长期记忆。
	if err := callMemory(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "c1", Role: "user", Content: "今天有点热"}, nil); err != nil {
		t.Fatal(err)
	}
	pid = pendingID(t, st)
	stub.setText(fmt.Sprintf(`[{"id":%d,"keep":true,"kind":"事件","content":"今天有点热","salience":0.2,"priority":0,"durability":"transient"}]`, pid))
	res, err = consolidateUntilReady(t, srv.WsURL())
	if err != nil || res.Skipped != 1 {
		t.Fatalf("transient 应跳过: %+v err=%v", res, err)
	}

	// 模型输出坏掉：整批不落共享库，候选留在池里重试。
	stub.setText("这不是 JSON")
	if err := callMemory(t, srv.WsURL(), "store", protocol.StorePayload{ConversationID: "c1", Role: "user", Content: "再记一条"}, nil); err != nil {
		t.Fatal(err)
	}
	var dummy protocol.ConsolidateResult
	if err := callMemory(t, srv.WsURL(), "consolidate", struct{}{}, &dummy); err == nil {
		t.Fatal("模型输出坏掉时应报错")
	}
	if pending, done, skipped, _, err := st.StagingCounts(); err != nil || pending != 1 || done != 1 || skipped != 2 {
		t.Fatalf("失败候选应留在 pending: pending=%d done=%d skipped=%d err=%v", pending, done, skipped, err)
	}
}
