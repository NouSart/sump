package tests

// stubMemory 是记忆服务替身（完全启动链）：recall → 注入固定记忆块 → 以 resume 发回 reasoner；
// store → 记录（供断言）。

import (
	"context"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

type stubMemory struct {
	ln  net.Listener
	srv *http.Server

	mu          sync.Mutex
	name        string // 服务名（默认 memory；服务名热改测试用别的名字）
	recalls     int    // 收到的 recall 次数
	reasonerURL string // resume 的投递目标（reasoner 启动后设置）
	stores      []protocol.StorePayload

	task     *protocol.Task // 非空时随 resume 带回（模拟"记忆已经确定了当前任务"）
	finished []protocol.TaskFinishPayload
	renamed  []protocol.TaskRenamePayload
	notes    []protocol.NotePayload // 复盘写回的经验（交付后由 reasoner 异步发来）
}

// startStubMemory 起一个服务名为 "memory" 的记忆替身。
func startStubMemory(t *testing.T, center *stubCenter) *stubMemory {
	t.Helper()
	return startStubMemoryNamed(t, center, "memory")
}

// startStubMemoryNamed 起一个自定义服务名的记忆替身（服务名热改测试用）。
func startStubMemoryNamed(t *testing.T, center *stubCenter, name string) *stubMemory {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	m := &stubMemory{ln: ln, name: name}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", m.handleWS)
	m.srv = &http.Server{Handler: mux}
	go func() { _ = m.srv.Serve(ln) }()
	t.Cleanup(func() { _ = m.srv.Close() })

	// 把记忆名片放进中心（reasoner 从 roster 解析它的地址）。
	center.mu.Lock()
	center.cards[name] = protocol.ServiceCard{
		Name:        name,
		Addr:        m.URL(),
		Description: "记忆测试替身",
		Provides: []protocol.Provide{
			{Action: "recall"}, {Action: "store"}, {Action: "task_finish"},
			{Action: "task_rename"}, {Action: "note"},
		},
	}
	center.mu.Unlock()
	return m
}

func (m *stubMemory) URL() string { return "ws://" + m.ln.Addr().String() + "/ws" }

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

// Notes 返回已收到的 note（复盘写回的经验）快照。
func (m *stubMemory) Notes() []protocol.NotePayload {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]protocol.NotePayload, len(m.notes))
	copy(out, m.notes)
	return out
}

func (m *stubMemory) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		switch jp.Action {
		case "store":
			var p protocol.StorePayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			m.mu.Lock()
			m.stores = append(m.stores, p)
			m.mu.Unlock()
			resp, _ := protocol.NewResponse(env, m.name, protocol.ResponsePayload{OK: true})
			c.send(resp)
		case "recall":
			var p protocol.RecallPayload
			if err := protocol.DecodeRaw(jp.Input, &p); err != nil {
				continue
			}
			m.mu.Lock()
			m.recalls++
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
			m.mu.Lock()
			task := m.task
			m.mu.Unlock()
			raw, err := protocol.EncodePayload(protocol.ResumePayload{Context: protocol.TaskContext{
				ConversationID: p.Context.ConversationID,
				Messages:       msgs,
				Boss:           p.Context.Boss,
				Task:           task,
			}})
			if err != nil {
				continue
			}
			resp, _ := protocol.NewResponse(env, m.name, protocol.ResponsePayload{OK: true})
			c.send(resp)
			m.fireResume(env.Trace, raw)
		case "task_finish":
			var p protocol.TaskFinishPayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			m.mu.Lock()
			m.finished = append(m.finished, p)
			m.mu.Unlock()
			resp, _ := protocol.NewResponse(env, m.name, protocol.ResponsePayload{OK: true})
			c.send(resp)
		case "task_rename":
			var p protocol.TaskRenamePayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			m.mu.Lock()
			m.renamed = append(m.renamed, p)
			m.mu.Unlock()
			resp, _ := protocol.NewResponse(env, m.name, protocol.ResponsePayload{OK: true})
			c.send(resp)
		case "note":
			var p protocol.NotePayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			m.mu.Lock()
			m.notes = append(m.notes, p)
			m.mu.Unlock()
			resp, _ := protocol.NewResponse(env, m.name, protocol.ResponsePayload{OK: true})
			c.send(resp)
		}
	}
}

// fireResume 把组装好的上下文发给 reasoner（from=memory → 触发完全启动）。
func (m *stubMemory) fireResume(trace string, raw msgpack.RawMessage) {
	m.mu.Lock()
	url := m.reasonerURL
	m.mu.Unlock()
	if url == "" {
		return
	}
	go func() {
		c, err := protocol.Dial(url, nil)
		if err != nil {
			return
		}
		defer c.Close()
		env, err := protocol.NewEnvelope(protocol.TypeJump, m.name, "reasoner", trace, protocol.JumpPayload{Action: "resume", Input: raw})
		if err != nil {
			return
		}
		env.Boss = "reasoner"
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = c.Call(ctx, env)
	}()
}
