package tests

// stubMemory 是记忆服务替身（完全启动链）：recall → 注入固定记忆块 → 以 resume 发回 reasoner；
// store → 记录（供断言）。全部走原生 /spv2。

import (
	"context"
	"sync"
	"testing"
	"time"

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
		spv2.Method{Name: "recall", Shape: spv2.ShapeCall},
		spv2.Method{Name: "store", Shape: spv2.ShapeCall},
		spv2.Method{Name: "task_finish", Shape: spv2.ShapeCall},
		spv2.Method{Name: "task_rename", Shape: spv2.ShapeCall},
		spv2.Method{Name: "note", Shape: spv2.ShapeCall},
		spv2.Method{Name: "publish_session", Shape: spv2.ShapeCall},
	)
	m := &stubMemory{srv: e.srv, url: e.URL(), name: name}

	e.handle("store", func(_ context.Context, call *spv2.Call) error {
		var p protocol.StorePayload
		_ = call.Frame.DecodeBody(&p)
		m.mu.Lock()
		m.stores = append(m.stores, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("recall", func(_ context.Context, call *spv2.Call) error {
		var p protocol.RecallPayload
		if err := call.Frame.DecodeBody(&p); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		m.mu.Lock()
		m.recalls++
		m.recallContexts = append(m.recallContexts, p.Context)
		task := m.task
		m.mu.Unlock()
		// 注入固定记忆块（插在第一条 system 之后）。
		msgs := make([]protocol.Message, 0, len(p.Context.Messages)+1)
		placed := false
		for _, msg := range p.Context.Messages {
			msgs = append(msgs, msg)
			if !placed && msg.Role == "system" {
				msgs = append(msgs, protocol.Message{Role: "system", Content: "【记忆】\n测试记忆块"})
				placed = true
			}
		}
		if err := call.Reply(map[string]any{"ok": true}); err != nil {
			return err
		}
		m.fireResume(call.Frame.Work, protocol.ResumePayload{Context: protocol.TaskContext{
			ConversationID: p.Context.ConversationID,
			Messages:       msgs,
			Boss:           p.Context.Boss,
			Task:           task,
		}})
		return nil
	})
	e.handle("task_finish", func(_ context.Context, call *spv2.Call) error {
		var p protocol.TaskFinishPayload
		_ = call.Frame.DecodeBody(&p)
		m.mu.Lock()
		m.finished = append(m.finished, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("task_rename", func(_ context.Context, call *spv2.Call) error {
		var p protocol.TaskRenamePayload
		_ = call.Frame.DecodeBody(&p)
		m.mu.Lock()
		m.renamed = append(m.renamed, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("note", func(_ context.Context, call *spv2.Call) error {
		var p protocol.NotePayload
		_ = call.Frame.DecodeBody(&p)
		m.mu.Lock()
		m.notes = append(m.notes, p)
		m.mu.Unlock()
		return call.Reply(map[string]any{"ok": true})
	})
	e.handle("publish_session", func(_ context.Context, call *spv2.Call) error {
		var p protocol.PublishSessionPayload
		_ = call.Frame.DecodeBody(&p)
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
			{Action: "recall"}, {Action: "store"}, {Action: "task_finish"},
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

// setTask 设置随 resume 带回的任务（nil = 没有当前任务）。
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
func (m *stubMemory) fireResume(trace string, p protocol.ResumePayload) {
	m.mu.Lock()
	url := m.reasonerURL
	m.mu.Unlock()
	if url == "" {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		c, err := spv2.Dial(ctx, url)
		if err != nil {
			return
		}
		defer c.Close()
		f, err := spv2.NewCall(trace, spv2.ServiceRef(m.name), spv2.ServiceRef("reasoner"), "resume", p)
		if err != nil {
			return
		}
		_, _ = c.Call(ctx, f)
	}()
}
