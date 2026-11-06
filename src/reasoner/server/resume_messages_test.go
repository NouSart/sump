package server

// resume 的任务提示插入位置（特征化测试，钉住「实际发生什么」）：
//
// 背景：审计报告认为 infer.go 的 resume「假定 Messages[0] 是 system，user 开头的链
// 首条被无声顶掉」。实测不成立——那一行是
//
//	tc.Messages = append(tc.Messages[:1:1], append([]llm.Message{{Role:"system", ...}}, tc.Messages[1:]...)...)
//
// 三值切片 `[:1:1]` 把容量截成 1，外层 append 必然另起一块新数组，所以第 0 条被原样
// 保留、任务提示插在索引 1。本文件把这个真实形状钉住：
//   - 一条不丢、一条不被覆盖（内容逐字保留）；
//   - 角色顺序 = [原第 0 条, system(任务提示), 其余原样]，即使第 0 条是 user；
//   - 没有任务 id 时一条都不插（今天的每个生产者都先给 system，见 infer.go:364 / hold.go:273 / async.go:310）。

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
)

// resumeQuietServer 起一个不接模型的 reasoner：boardKick 会开一轮，但那一轮看到
// 「LLM 未配置」立刻返回，不往黑板里追加任何东西——消息链因此保持 resume 写入时的形状，
// 断言才是确定的（不需要等模型，也不会被后台轮次改写）。
func resumeQuietServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{Name: "reasoner", SecretFile: "-"}, log.New(io.Discard, "", 0))
}

// resumeToBoard 以记忆服务的身份走一次 resume（from=memory 才受理），返回黑板里那个任务。
// 返回的是 taskState：调用方要用 boardMessages 读消息链（后台轮次可能并发读写）。
func resumeToBoard(t *testing.T, s *Server, cid string, msgs []llm.Message, task *protocol.Task) *taskState {
	t.Helper()
	raw, err := protocol.EncodePayload(protocol.ResumePayload{Context: protocol.TaskContext{
		ConversationID: cid,
		Messages:       msgs,
		Boss:           "boss-from-context",
		Task:           task,
	}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.resume("memory", "boss-from-frame", "trace-resume-1", raw)
	if err != nil {
		t.Fatalf("resume 被拒: %v", err)
	}
	if got, _ := out.(map[string]any); got["status"] != "accepted" {
		t.Fatalf("resume 回执不符: %+v", out)
	}
	st := s.boardGet(cid)
	if st == nil {
		t.Fatalf("resume 之后黑板里没有会话 %s", cid)
	}
	return st
}

// boardMessages 加锁取消息链快照（后台那一轮只是读，但测试仍要按同一把锁读）。
func boardMessages(st *taskState) []llm.Message {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]llm.Message, len(st.messages))
	copy(out, st.messages)
	return out
}

// rolesOf 把消息链压成角色序列，便于把「顺序」这件事一次说清。
func rolesOf(msgs []llm.Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		out[i] = m.Role
	}
	return out
}

// assertKept 断言 got[i] 就是当初传进来的 want：角色与正文逐字相同，没被覆盖、没被改写。
func assertKept(t *testing.T, got []llm.Message, i int, want llm.Message) {
	t.Helper()
	if i >= len(got) {
		t.Fatalf("第 %d 条被丢了：整条链 %+v（want %+v）", i, rolesOf(got), want)
	}
	if got[i].Role != want.Role || got[i].Content != want.Content {
		t.Fatalf("第 %d 条不是原样：得到 role=%q content=%q，应为 role=%q content=%q",
			i, got[i].Role, got[i].Content, want.Role, want.Content)
	}
}

// TestResumeUserFirstChainKeepsFirstMessage 是本次的核心用例：上下文以 user 开头时，
// 任务提示插在索引 1，第 0 条用户消息原样留着。
func TestResumeUserFirstChainKeepsFirstMessage(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{
		{Role: "user", Content: "第一条就是用户先开口"},
		{Role: "assistant", Content: "上一轮的助手发言"},
		{Role: "user", Content: "这一轮用户又说了"},
	}
	st := resumeToBoard(t, s, "c-user-first", in, &protocol.Task{ID: "task-7", Name: "排查端口"})

	got := boardMessages(st)
	// 实际结果的角色顺序（不是「[0] 一定是 system」）：任务提示夹在第 0 条之后。
	if strings.Join(rolesOf(got), ",") != "user,system,assistant,user" {
		t.Fatalf("角色顺序应为 [user, system, assistant, user]，实际 %+v（消息 %+v）", rolesOf(got), got)
	}
	if len(got) != len(in)+1 {
		t.Fatalf("应只多出 1 条任务提示：得到 %d 条，输入 %d 条", len(got), len(in))
	}
	// 一条不丢、一条不被覆盖。
	assertKept(t, got, 0, in[0])
	assertKept(t, got, 2, in[1])
	assertKept(t, got, 3, in[2])
	// 多出来的那条是任务提示，位置在索引 1。
	if got[1].Role != "system" || !strings.Contains(got[1].Content, "当前任务：《排查端口》（id=task-7）") {
		t.Fatalf("索引 1 应是任务提示: %+v", got[1])
	}
	if strings.Contains(got[0].Content, "当前任务：") {
		t.Fatalf("第 0 条被任务提示顶掉了（审计报告描述的行为，不该发生）: %q", got[0].Content)
	}
}

// TestResumeSystemFirstChainKeepsSystemPromptAtZero 记录今天真实的形状：
// 生产者给的上下文以 system 开头，于是插入后是两条 system（骨架提示 + 任务提示）。
// 第 0 条仍然是原文，不是被覆盖后的任务提示。
func TestResumeSystemFirstChainKeepsSystemPromptAtZero(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "现在几点了"},
	}
	st := resumeToBoard(t, s, "c-system-first", in, &protocol.Task{ID: "task-8", Name: "查时间"})

	got := boardMessages(st)
	if strings.Join(rolesOf(got), ",") != "system,system,user" {
		t.Fatalf("角色顺序应为 [system, system, user]，实际 %+v", rolesOf(got))
	}
	if got[0].Content != systemPrompt {
		t.Fatalf("索引 0 必须还是原 system 提示（逐字）: %q", clip(got[0].Content, 120))
	}
	if !strings.Contains(got[1].Content, "当前任务：《查时间》（id=task-8）") {
		t.Fatalf("索引 1 应是任务提示: %q", got[1].Content)
	}
	assertKept(t, got, 2, in[1])
}

// TestResumeWithoutTaskInsertsNothing 钉住门禁：没有任务 id 就不插任务提示，
// 消息链原样进黑板（否则「插入」这件事本身会变成第二条要守的规矩）。
func TestResumeWithoutTaskInsertsNothing(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{
		{Role: "user", Content: "用户先开口"},
		{Role: "assistant", Content: "助手回了一句"},
	}
	st := resumeToBoard(t, s, "c-no-task", in, nil)

	got := boardMessages(st)
	if strings.Join(rolesOf(got), ",") != "user,assistant" {
		t.Fatalf("没有任务时不该插入任何消息，实际 %+v", rolesOf(got))
	}
	assertKept(t, got, 0, in[0])
	assertKept(t, got, 1, in[1])
}

// TestResumeAdoptsFrameWorkAsChainTrace 说明 resume 后这条链的 trace 从哪来：
// 帧头 work 成为任务的 trace，上下文里的 boss 优先、缺省才用帧头 from。
// （tests/ 里的全链断言依赖这个：work 不变，deliver/store/note 才谈得上「同一个工作号」。）
func TestResumeAdoptsFrameWorkAsChainTrace(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: "继续"}}
	st := resumeToBoard(t, s, "c-trace-keep", in, &protocol.Task{ID: "task-9", Name: "续跑"})

	st.mu.Lock()
	trace, boss, taskID := st.trace, st.boss, st.taskID
	st.mu.Unlock()
	if trace != "trace-resume-1" {
		t.Fatalf("任务的 trace 应等于帧头 work，实际 %q", trace)
	}
	if boss != "boss-from-context" {
		t.Fatalf("上下文带 boss 时应优先用它，实际 %q", boss)
	}
	if taskID != "task-9" {
		t.Fatalf("任务 id 应随上下文恢复，实际 %q", taskID)
	}
}
