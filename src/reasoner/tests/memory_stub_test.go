package tests

// stubMemory 是记忆服务替身（完全启动链）：recall → 注入固定记忆块 → 以 resume 发回 reasoner；
// store → 记录（供断言）。全部走原生 /spv2。

import (
	"context"
	"sync"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

type stubMemory struct {
	srv *spv2.Server
	url string

	mu          sync.Mutex
	name        string // 服务名（默认 memory；服务名热改测试用别的名字）
	recalls     int    // 收到的 recall 次数
	reasonerURL string // resume 的投递目标（reasoner 启动后设置）
	stores      []protocol.StorePayload
	// recallContexts 是收到的 recall 上下文（断言"reasoner 把任务名托管过来"）。
	recallContexts []protocol.TaskContext

	task     *protocol.Task // 非空时随 resume 带回（模拟"记忆已经确定了当前任务"）
	finished []protocol.TaskFinishPayload
	renamed  []protocol.TaskRenamePayload
	notes    []protocol.NotePayload // 复盘写回的经验（交付后由 reasoner 异步发来）
	// publishes 是复盘发布到共享经验库的任务摘要（另一个库、另一条路）。
	publishes []protocol.PublishSessionPayload
	// works 记录每次入站调用的帧头 work（SPv2 的链路 id / trace）：
	// 「trace 原样沿链」只有在对端留下记录之后才是可断言的事实。
	works map[string][]string
	// contextOverride 非空时 context 直接回这组消息（测试消息链形状用）。
	contextOverride []protocol.Message
	// A2A 队列替身：message_post/claim/ack。
	a2aQueue    map[string][]protocol.A2AMessage
	a2aClaimed  map[string]string // message id → work id
	a2aClaimErr error             // 非 nil 时 message_claim 直接失败（测 claim 失败指标）
	// lease 账本替身：reasoner 每次真执行远端工具前都要扣一次 tool_calls。
	leaseCharges []spv2.Charge
	workStatuses []string
	leaseErr     string // 非空时 lease_charge 一律回这个错误（验证"扣不到租约就不执行工具"）
}

// LeaseCharges 返回按到达顺序的租约扣减快照。
func (m *stubMemory) LeaseCharges() []spv2.Charge {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]spv2.Charge(nil), m.leaseCharges...)
}

// WorkStatuses 返回按到达顺序的 work_update 状态快照。
func (m *stubMemory) WorkStatuses() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.workStatuses...)
}

// setLeaseError 让后续 lease_charge 失败（空串 = 恢复正常）。
func (m *stubMemory) setLeaseError(msg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.leaseErr = msg
}

// noteWork 记下这帧的 work。各 handler 第一件事就做它，确保测试观察到结果时记录已就位。
func (m *stubMemory) noteWork(method, work string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.works == nil {
		m.works = map[string][]string{}
	}
	m.works[method] = append(m.works[method], work)
}

// Works 返回某 method 收到的帧头 work 快照（按到达顺序；没有则空）。
func (m *stubMemory) Works(method string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.works[method]...)
}

// startStubMemory 起一个服务名为 "memory" 的记忆替身。
func startStubMemory(t *testing.T, center *stubCenter) *stubMemory {
	t.Helper()
	return startStubMemoryNamed(t, center, "memory")
}

// startStubMemoryNamed 起一个自定义服务名的记忆替身（服务名热改测试用）。
func startStubMemoryNamed(t *testing.T, center *stubCenter, name string) *stubMemory {
	t.Helper()
	e := newStubEndpoint(t, name,
		spv2.Method{Name: "context", Shape: spv2.ShapeCall},
		spv2.Method{Name: "store", Shape: spv2.ShapeCall},
		spv2.Method{Name: "task_finish", Shape: spv2.ShapeCall},
		spv2.Method{Name: "task_rename", Shape: spv2.ShapeCall},
		spv2.Method{Name: "note", Shape: spv2.ShapeCall},
		spv2.Method{Name: "publish_session", Shape: spv2.ShapeCall},
		spv2.Method{Name: "lease_charge", Shape: spv2.ShapeCall},
		spv2.Method{Name: "lease_release", Shape: spv2.ShapeCall},
		spv2.Method{Name: "work_update", Shape: spv2.ShapeCall},
		spv2.Method{Name: "message_post", Shape: spv2.ShapeCall},
		spv2.Method{Name: "message_claim", Shape: spv2.ShapeCall},
		spv2.Method{Name: "message_ack", Shape: spv2.ShapeCall},
	)
	m := &stubMemory{srv: e.srv, url: e.URL(), name: name, works: map[string][]string{}, a2aQueue: map[string][]protocol.A2AMessage{}, a2aClaimed: map[string]string{}}

	e.handle("store", func(_ context.Context, call *spv2.Call) error {
		var p protocol.StorePayload
		_ = call.Frame.DecodeBody(&p)
		m.noteWork("store", call.Frame.Work)
		m.mu.Lock()
		m.stores = append(m.stores, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("context", func(_ context.Context, call *spv2.Call) error {
		var p protocol.ContextPayload
		if err := call.Frame.DecodeBody(&p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.noteWork("context", call.Frame.Work)
		tc := protocol.TaskContext{
			ConversationID: p.Conversation,
			Messages:       p.Messages,
			Boss:           p.Boss,
			TaskName:       p.TaskName,
		}
		m.mu.Lock()
		m.recalls++
		m.recallContexts = append(m.recallContexts, tc)
		task := m.task
		override := append([]protocol.Message(nil), m.contextOverride...)
		m.mu.Unlock()
		// 与真实 memory 一样：在第一条 system 后注入记忆块，并带回 work/task。
		if len(override) > 0 {
			return call.Reply(map[string]any{
				"messages": override,
				"work":     map[string]any{"id": call.Frame.Work, "status": "running"},
				"task":     task,
			})
		}
		msgs := make([]protocol.Message, 0, len(tc.Messages)+1)
		placed := false
		for _, msg := range tc.Messages {
			msgs = append(msgs, msg)
			if !placed && msg.Role == "system" {
				msgs = append(msgs, protocol.Message{Role: "system", Content: "【记忆】\n测试记忆块"})
				placed = true
			}
		}
		return call.Reply(map[string]any{
			"messages": msgs,
			"work":     map[string]any{"id": call.Frame.Work, "status": "running"},
			"task":     task,
		})
	})
	e.handle("message_post", func(_ context.Context, call *spv2.Call) error {
		var req struct {
			Work string `msgpack:"work"`
			Body string `msgpack:"body"`
		}
		if err := call.Frame.DecodeBody(&req); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.noteWork("message_post", call.Frame.Work)
		msg := protocol.A2AMessage{ID: "a2a-" + call.Frame.ID, WorkID: req.Work, From: protocol.ActorRef{Kind: "agent", ID: call.Frame.From.ID}, Body: req.Body}
		m.mu.Lock()
		m.a2aQueue[req.Work] = append(m.a2aQueue[req.Work], msg)
		m.mu.Unlock()
		return call.Reply(map[string]any{"message": msg})
	})
	e.handle("message_claim", func(_ context.Context, call *spv2.Call) error {
		var req struct {
			Work string `msgpack:"work"`
		}
		if err := call.Frame.DecodeBody(&req); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.noteWork("message_claim", call.Frame.Work)
		m.mu.Lock()
		claimErr := m.a2aClaimErr
		m.mu.Unlock()
		if claimErr != nil {
			return call.ReplyError(spv2.CodeInternal, claimErr.Error(), true, nil)
		}
		m.mu.Lock()
		queue := m.a2aQueue[req.Work]
		if len(queue) == 0 {
			m.mu.Unlock()
			return call.ReplyError(spv2.CodeNotFound, "没有可领取的消息", false, nil)
		}
		msg := queue[0]
		m.a2aQueue[req.Work] = queue[1:]
		m.a2aClaimed[msg.ID] = req.Work
		m.mu.Unlock()
		return call.Reply(map[string]any{"message": msg})
	})
	e.handle("message_ack", func(_ context.Context, call *spv2.Call) error {
		var req struct {
			ID string `msgpack:"id"`
		}
		if err := call.Frame.DecodeBody(&req); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.mu.Lock()
		delete(m.a2aClaimed, req.ID)
		m.mu.Unlock()
		return call.Reply(map[string]any{"acked": true})
	})
	e.handle("lease_charge", func(_ context.Context, call *spv2.Call) error {
		var req struct {
			Work  string      `msgpack:"work"`
			Delta spv2.Charge `msgpack:"delta"`
		}
		if err := call.Frame.DecodeBody(&req); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.noteWork("lease_charge", call.Frame.Work)
		m.mu.Lock()
		m.leaseCharges = append(m.leaseCharges, req.Delta)
		errMsg := m.leaseErr
		m.mu.Unlock()
		if errMsg != "" {
			return call.ReplyError(spv2.CodeResourceExhausted, errMsg, true, nil)
		}
		return call.Reply(map[string]any{"remaining": spv2.Remaining{ToolCalls: 49}})
	})
	e.handle("lease_release", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(map[string]any{"released": true})
	})
	e.handle("work_update", func(_ context.Context, call *spv2.Call) error {
		var req struct {
			Work   string `msgpack:"work"`
			Status string `msgpack:"status"`
		}
		if err := call.Frame.DecodeBody(&req); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.noteWork("work_update", call.Frame.Work)
		m.mu.Lock()
		m.workStatuses = append(m.workStatuses, req.Status)
		m.mu.Unlock()
		return call.Reply(map[string]any{"status": req.Status})
	})
	e.handle("task_finish", func(_ context.Context, call *spv2.Call) error {
		var p protocol.TaskFinishPayload
		_ = call.Frame.DecodeBody(&p)
		m.noteWork("task_finish", call.Frame.Work)
		m.mu.Lock()
		m.finished = append(m.finished, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("task_rename", func(_ context.Context, call *spv2.Call) error {
		var p protocol.TaskRenamePayload
		_ = call.Frame.DecodeBody(&p)
		m.noteWork("task_rename", call.Frame.Work)
		m.mu.Lock()
		m.renamed = append(m.renamed, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("note", func(_ context.Context, call *spv2.Call) error {
		var p protocol.NotePayload
		_ = call.Frame.DecodeBody(&p)
		m.noteWork("note", call.Frame.Work)
		m.mu.Lock()
		m.notes = append(m.notes, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("publish_session", func(_ context.Context, call *spv2.Call) error {
		var p protocol.PublishSessionPayload
		_ = call.Frame.DecodeBody(&p)
		m.noteWork("publish_session", call.Frame.Work)
		m.mu.Lock()
		m.publishes = append(m.publishes, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})

	// 把记忆名片注册进中心（reasoner 从 roster 解析它的地址）。
	registerStub(t, center, protocol.RegisterPayload{
		Name:        name,
		Addr:        e.URL(),
		Description: "记忆测试替身",
		Provides: []protocol.Provide{
			{Action: "context"}, {Action: "recall"}, {Action: "store"}, {Action: "task_finish"},
			{Action: "task_rename"}, {Action: "note"}, {Action: "publish_session"},
		},
	})
	return m
}

func (m *stubMemory) URL() string { return m.url }

func (m *stubMemory) setReasonerURL(u string) {
	m.mu.Lock()
	m.reasonerURL = u
	m.mu.Unlock()
}

// setContextOverride 让 context 直接回这组消息（nil = 走默认注入逻辑）。
func (m *stubMemory) setContextOverride(msgs []protocol.Message) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.contextOverride = append([]protocol.Message(nil), msgs...)
}

// setTask 设置随 context 带回的任务（nil = 没有当前任务）。
func (m *stubMemory) setTask(t *protocol.Task) {
	m.mu.Lock()
	m.task = t
	m.mu.Unlock()
}

// Finished 返回收到的 task_finish（任务完结）。
func (m *stubMemory) Finished() []protocol.TaskFinishPayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.TaskFinishPayload, len(m.finished))
	copy(out, m.finished)
	return out
}

// Renamed 返回收到的 task_rename（任务改名）。
func (m *stubMemory) Renamed() []protocol.TaskRenamePayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.TaskRenamePayload, len(m.renamed))
	copy(out, m.renamed)
	return out
}

// Stores 返回已收到的 store 请求快照。
func (m *stubMemory) Stores() []protocol.StorePayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.StorePayload, len(m.stores))
	copy(out, m.stores)
	return out
}

// Recalls 返回已收到的 recall 次数。
func (m *stubMemory) Recalls() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recalls
}

// RecallContexts 返回已收到的 recall 上下文快照（断言"任务名也托管过去了"）。
func (m *stubMemory) RecallContexts() []protocol.TaskContext {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.TaskContext, len(m.recallContexts))
	copy(out, m.recallContexts)
	return out
}

// Notes 返回已收到的 note（复盘写回的经验）快照。
func (m *stubMemory) Notes() []protocol.NotePayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.NotePayload, len(m.notes))
	copy(out, m.notes)
	return out
}

// Publishes 返回已收到的 publish_session（共享库的会话摘要）快照。
func (m *stubMemory) Publishes() []protocol.PublishSessionPayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.PublishSessionPayload, len(m.publishes))
	copy(out, m.publishes)
	return out
}

// fireResume 把组装好的上下文发给 reasoner（from=memory → 触发完全启动）。
// work 沿用 recall 的 trace，整条链仍是同一个工作号。

// A2AQueueLen 返回某 work 上尚未领取的消息数。
func (m *stubMemory) A2AQueueLen(work string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.a2aQueue[work])
}

// A2AClaimedCount 返回已 claim 未 ack 的消息数。
func (m *stubMemory) A2AClaimedCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.a2aClaimed)
}

// SetA2AClaimError 让 message_claim 直接回 internal 错误（测 claim 失败指标/降级可见性）。
func (m *stubMemory) SetA2AClaimError(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.a2aClaimErr = err
}
