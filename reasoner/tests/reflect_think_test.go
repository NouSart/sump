package tests

// 复盘与单步推理在**服务层**的契约（进程内真服务 + 真 WS，不是纯函数单测）：
//
//   - think 真的能调通，且**不出现在给模型的工具列表里**（模型不能自己让自己再想一次）；
//   - 交付后复盘真的把经验发给了记忆服务（带 task_id 可回溯），多条各发一条；
//   - 关掉 reflect.on 就一条都不发；
//   - 复盘自己失败（上游 500）**不影响交付**——老板拿到答复这件事与复盘无关。
//
// 最后一条是这套设计的承诺，也是最容易在重构里被打破的一条：复盘在 goroutine 里，
// 一旦有人把它挪回同步路径，交付就会被一个挂掉的模型带下水。

import (
	"context"
	"errors"
	"log"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/reasoner/llm"
	"github.com/QAQ-awa-QAQ/sump/reasoner/server"
)

// scriptedLLM 按请求形状分派答复，并能对复盘返回错误：
//
//	复盘（system 里带"复盘器"） → notes JSON（或错误）
//	不带工具（就是 think）      → thinkReply
//	其余（任务轮次）            → 判定 / 答复交替
type scriptedLLM struct {
	mu         sync.Mutex
	turns      int    // 任务轮次计数
	reflects   int    // 复盘次数
	notes      string // 复盘时吐的 JSON
	thinkReply string // think 时回的话
	failWith   error  // 复盘时返回这个错误（nil = 正常）
	seen       []llm.ChatRequest
}

func (s *scriptedLLM) Chat(_ context.Context, req llm.ChatRequest) (llm.Message, error) {
	isReflect := false
	for _, m := range req.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "复盘器") {
			isReflect = true
			break
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seen = append(s.seen, req)
	switch {
	case isReflect:
		s.reflects++
		if s.failWith != nil {
			return llm.Message{}, s.failWith
		}
		return llm.Message{Role: "assistant", Content: s.notes}, nil
	case len(req.Tools) == 0:
		// think：单步推理不带工具。
		return llm.Message{Role: "assistant", Content: s.thinkReply}, nil
	}
	s.turns++
	if s.turns%2 == 1 {
		return llm.Message{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID: "call_judge", Type: "function",
			Function: llm.FunctionCall{Name: "task_status", Arguments: `{"status":"done"}`},
		}}}, nil
	}
	return llm.Message{Role: "assistant", Content: "任务答复"}, nil
}

func (s *scriptedLLM) Reflects() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reflects
}

func (s *scriptedLLM) Requests() []llm.ChatRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]llm.ChatRequest, len(s.seen))
	copy(out, s.seen)
	return out
}

// startReflectEnv 起一套最小环境：中心 / boss / 记忆替身 / reasoner（不给慢工具——这里不调工具）。
func startReflectEnv(t *testing.T, sc *stubCenter, client llm.Client, opts ...func(*server.Config)) (*server.Server, *stubBoss, *stubMemory) {
	t.Helper()
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)

	cfg := server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		NotifyMode:        "each",
		LLM:               client,
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	s := server.New(cfg, log.New(os.Stdout, "[reflect-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel) // ctx 需活到测试结束：骨架的后台工作（心跳 / 设置拉取）挂在它上面
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())
	return s, boss, mem
}

// noteEventually 等记忆替身收到 note（复盘是交付后的异步动作，必须轮询）。
func noteEventually(t *testing.T, m *stubMemory, want int, timeout time.Duration) []protocol.NotePayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := m.Notes(); len(got) >= want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	return m.Notes()
}

// toolNames 把一次请求里的工具名抓出来。
func toolNames(req llm.ChatRequest) []string {
	out := make([]string, 0, len(req.Tools))
	for _, tl := range req.Tools {
		out = append(out, tl.Function.Name)
	}
	return out
}

// TestThinkActionServed 验证 think 在服务层可用，且它是"一次补全"而不是任务。
func TestThinkActionServed(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{thinkReply: "一句话答案"}
	s, _, _ := startReflectEnv(t, sc, client)

	c := dialReasoner(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rp := jump(t, c, "think", protocol.ThinkPayload{Prompt: "说一句话"}, ctx)
	if !rp.OK {
		t.Fatalf("think 失败: %s", rp.Error)
	}
	var out protocol.ThinkResult
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatal(err)
	}
	if out.Text != "一句话答案" {
		t.Fatalf("think 应原样回模型那段文本: %+v", out)
	}
	if client.Reflects() != 0 {
		t.Fatalf("think 不该触发复盘，实际 %d 次", client.Reflects())
	}
	if client.turns != 0 {
		t.Fatalf("think 不该走任务轮次，实际走了 %d 轮", client.turns)
	}
}

// TestThinkNotAdvertisedToModel 验证 think **不在**给模型的工具列表里。
// 服务层可用 ≠ 该给模型用：它是"让自己再想一次"，暴露出去就是自我循环的起点。
func TestThinkNotAdvertisedToModel(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{}
	s, boss, _ := startReflectEnv(t, sc, client)

	sendUser(t, s, "随便问一句")
	waitDelivery(t, boss, "任务答复")

	reqs := client.Requests()
	if len(reqs) == 0 {
		t.Fatal("模型没收到任何请求")
	}
	names := toolNames(reqs[0])
	if len(names) == 0 {
		t.Fatal("第一轮应该带工具列表（至少内置 task_status / wait）")
	}
	for _, n := range names {
		if strings.Contains(n, "think") {
			t.Fatalf("think 不该出现在工具列表里: %v", names)
		}
	}
}

// TestReflectSendsNotes 验证交付后真的把经验发给记忆（带 task_id），多条各发一条。
func TestReflectSendsNotes(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{notes: `{"notes":[
		{"content":"并发写要带会话内单调序号","kind":"坑"},
		{"content":"用户喜欢先看结论","kind":"偏好"}
	]}`}
	s, boss, mem := startReflectEnv(t, sc, client, func(c *server.Config) { c.Reflect = true })
	// 真实链路里任务由记忆服务在 recall 时确定；这里让替身也带回一个任务，
	// 才能断言"经验带得上 task_id"（没任务的会话本来就没有可回溯的存档）。
	mem.setTask(&protocol.Task{ID: "task-reflect", ConversationID: "default", Name: "修序号", Status: "active"})

	sendUser(t, s, "把序号问题修一下")
	waitDelivery(t, boss, "任务答复")

	notes := noteEventually(t, mem, 2, 10*time.Second)
	if len(notes) != 2 {
		t.Fatalf("应写回 2 条经验，实际 %d 条: %+v", len(notes), notes)
	}
	// 顺序不定：Fire 各自成 goroutine（与 timer 的补报同理），所以按内容找，不按位置断言。
	byContent := map[string]protocol.NotePayload{}
	for _, n := range notes {
		byContent[n.Content] = n
	}
	pit, ok := byContent["并发写要带会话内单调序号"]
	if !ok || pit.Durability != "durable" {
		t.Fatalf("坑应是 durable: %+v", notes)
	}
	pref, ok := byContent["用户喜欢先看结论"]
	if !ok || pref.Durability != "core" {
		t.Fatalf("偏好应是不该忘的 core: %+v", notes)
	}
	for _, n := range notes {
		if n.TaskID != "task-reflect" {
			t.Fatalf("经验必须带 task_id（可回溯到当时的存档）: %+v", n)
		}
		if n.ConversationID != "default" {
			t.Fatalf("经验应记下所属会话: %+v", n)
		}
	}
}

// TestReflectOffSendsNothing 验证 reflect.on 关掉之后一条都不发（不花钱、不写网）。
func TestReflectOffSendsNothing(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{notes: `{"notes":[{"content":"不该被写进去","kind":"坑"}]}`}
	s, boss, mem := startReflectEnv(t, sc, client) // Reflect 保持零值 = 关

	sendUser(t, s, "关掉复盘也要能正常交付")
	waitDelivery(t, boss, "任务答复")

	// 交付已经完成；再等一小会儿确认没有迟到的复盘。
	time.Sleep(500 * time.Millisecond)
	if got := mem.Notes(); len(got) != 0 {
		t.Fatalf("复盘关掉时不该写经验，实际 %+v", got)
	}
	if client.Reflects() != 0 {
		t.Fatalf("复盘关掉时不该发复盘请求，实际 %d 次", client.Reflects())
	}
}

// TestReflectFailureDoesNotBreakDelivery 验证复盘失败时交付照常完成、且不写经验。
// 这是"不能容忍一丁点阻塞"的具体断言：复盘在 goroutine 里，上游 500 与老板无关。
func TestReflectFailureDoesNotBreakDelivery(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{failWith: errors.New("上游 500")}
	s, boss, mem := startReflectEnv(t, sc, client, func(c *server.Config) { c.Reflect = true })

	sendUser(t, s, "复盘挂掉也不能影响交付")
	waitDelivery(t, boss, "任务答复") // 关键断言：答复照常到达

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && client.Reflects() == 0 {
		time.Sleep(50 * time.Millisecond)
	}
	if client.Reflects() == 0 {
		t.Fatal("复盘应该尝试过（失败也是尝试）")
	}
	if got := mem.Notes(); len(got) != 0 {
		t.Fatalf("复盘失败时不该写经验，实际 %+v", got)
	}
}
