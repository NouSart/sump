package experience

// 整理池的跨服务路径：真 /spv2 的假 session peer + 假 reasoner。
// experience 不打开会话库——候选与状态回写全部走 SPv2。

import (
	"context"
	"io"
	"log"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// startTestService 起一个独立模式的骨架服务（假对端）。
func startTestService(t *testing.T, svc *service.Service) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(svc.Shutdown)
}

// fakeSession 是一个真的 /spv2 服务：只实现整理池三个内部 method。
type fakeSession struct {
	mu      sync.Mutex
	items   []protocol.StagedItem
	marks   map[int64]string
	retries map[int64]string
	svc     *service.Service
}

func startFakeSession(t *testing.T, items []protocol.StagedItem) *fakeSession {
	t.Helper()
	fs := &fakeSession{items: items, marks: map[int64]string{}, retries: map[int64]string{}}
	fs.svc = service.New(service.Config{
		Name: "session", Listen: "127.0.0.1:0", Standalone: true,
		Logger: log.New(io.Discard, "", 0),
	})
	fs.svc.HandleNative(spv2.Method{Name: "staged_pending"}, func(_ context.Context, call *spv2.Call) error {
		fs.mu.Lock()
		defer fs.mu.Unlock()
		return call.Reply(protocol.StagedPendingResult{Items: append([]protocol.StagedItem(nil), fs.items...)})
	})
	fs.svc.HandleNative(spv2.Method{Name: "staged_mark"}, func(_ context.Context, call *spv2.Call) error {
		var p struct {
			ID      int64  `msgpack:"id"`
			Status  string `msgpack:"status"`
			LastErr string `msgpack:"last_error"`
		}
		if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, "staged_mark body 无效: "+err.Error(), false, nil)
		}
		fs.mu.Lock()
		fs.marks[p.ID] = p.Status
		fs.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	fs.svc.HandleNative(spv2.Method{Name: "staged_retry"}, func(_ context.Context, call *spv2.Call) error {
		var p struct {
			ID      int64  `msgpack:"id"`
			LastErr string `msgpack:"last_error"`
		}
		if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, "staged_retry body 无效: "+err.Error(), false, nil)
		}
		fs.mu.Lock()
		fs.retries[p.ID] = p.LastErr
		fs.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	startTestService(t, fs.svc)
	return fs
}

func (f *fakeSession) markOf(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.marks[id]
}

func (f *fakeSession) retryOf(id int64) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.retries[id]
}

// fakeReasoner 是一个真的 /spv2 服务：只实现 think。
type fakeReasoner struct {
	mu     sync.Mutex
	reply  string
	fail   bool
	prompt string
	svc    *service.Service
}

func startFakeReasoner(t *testing.T, reply string, fail bool) *fakeReasoner {
	t.Helper()
	fr := &fakeReasoner{reply: reply, fail: fail}
	fr.svc = service.New(service.Config{
		Name: "reasoner", Listen: "127.0.0.1:0", Standalone: true,
		Logger: log.New(io.Discard, "", 0),
	})
	fr.svc.HandleNative(spv2.Method{Name: "think"}, func(_ context.Context, call *spv2.Call) error {
		var p protocol.ThinkPayload
		if err := protocol.DecodeRaw(call.Frame.Body, &p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, "think body 无效: "+err.Error(), false, nil)
		}
		fr.mu.Lock()
		fr.prompt = p.Prompt
		reply, fail := fr.reply, fr.fail
		fr.mu.Unlock()
		if fail {
			return call.ReplyError(spv2.CodeInternal, "模型暂时不可用", true, nil)
		}
		return call.Reply(protocol.ThinkResult{Text: reply})
	})
	startTestService(t, fr.svc)
	return fr
}

func (f *fakeReasoner) lastPrompt() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prompt
}

// TestConsolidateViaFakeSessionPeer 钉住跨服务成功路径：
// 候选从 session 取（protocol.StagedItem）→ reasoner 分类 → 写共享长期记忆 → session 标记 done。
func TestConsolidateViaFakeSessionPeer(t *testing.T) {
	fs := startFakeSession(t, []protocol.StagedItem{{
		ID: 7, ConversationID: "c-1", Role: "user", Content: "用户住在杭州", Status: "pending",
	}})
	fr := startFakeReasoner(t,
		`[{"id":7,"keep":true,"kind":"身份","content":"用户住在杭州","salience":0.8,"priority":1,"durability":"durable","reason":"稳定事实"}]`,
		false)
	srv, sh := startExperience(t, Config{Peers: map[string]string{
		"session": fs.svc.WsURL(), "reasoner": fr.svc.WsURL(),
	}})

	body, err := call(t, dial(t, srv.WsURL()), spv2.ServiceRef("memory"), "trace-consolidate", "consolidate", map[string]any{})
	if err != nil {
		t.Fatalf("consolidate 失败: %v", err)
	}
	var res protocol.ConsolidateResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatal(err)
	}
	if res.Processed != 1 || res.Archived != 1 || res.Failed != 0 {
		t.Fatalf("整理结果不符: %+v", res)
	}
	if got := fs.markOf(7); got != "done" {
		t.Fatalf("候选应被标记 done，实际 %q", got)
	}
	if got := fs.retryOf(7); got != "" {
		t.Fatalf("成功路径不应重试: %q", got)
	}
	entries, err := sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Content != "用户住在杭州" || entries[0].Kind != "身份" {
		t.Fatalf("共享长期记忆应写入一条: %+v", entries)
	}
	if prompt := fr.lastPrompt(); !strings.Contains(prompt, "用户住在杭州") || !strings.Contains(prompt, "role=user") {
		t.Fatalf("提示词应包含候选原文与角色: %q", prompt)
	}
}

// TestConsolidateFailureKeepsPoolAndWritesNothing 钉住失败语义：
// reasoner 失败时候选回到 session 的整理池（retry），不写共享长期记忆。
func TestConsolidateFailureKeepsPoolAndWritesNothing(t *testing.T) {
	fs := startFakeSession(t, []protocol.StagedItem{{
		ID: 9, ConversationID: "c-2", Role: "user", Content: "一条不该被归档的观察", Status: "pending",
	}})
	fr := startFakeReasoner(t, "", true)
	srv, sh := startExperience(t, Config{Peers: map[string]string{
		"session": fs.svc.WsURL(), "reasoner": fr.svc.WsURL(),
	}})

	if _, err := call(t, dial(t, srv.WsURL()), spv2.ServiceRef("memory"), "trace-consolidate", "consolidate", map[string]any{}); err == nil {
		t.Fatal("reasoner 失败时 consolidate 应报错")
	}
	if got := fs.retryOf(9); got == "" {
		t.Fatal("失败候选应回写 staged_retry")
	}
	if got := fs.markOf(9); got != "" {
		t.Fatalf("失败候选不应被标记结束，实际 %q", got)
	}
	entries, err := sh.Longterm.ListActive()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("失败不写长期记忆，实际 %+v", entries)
	}
}
