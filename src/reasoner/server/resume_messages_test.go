package server

// 黑板消息链的形状（R-026 P2c 后的特征化测试）：
//
// 任务提示由 memory 的 assemble 写进基座（system），reasoner **不再二次插入**
// （旧 withTaskPrompt 已删除）。这里钉住：
//   - 一条不丢、一条不被覆盖（内容逐字保留）；
//   - 角色顺序 = 输入原样，即使第 0 条是 user 也不插 system、不顶掉；
//   - 没有任务 id 时同样原样。

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

// contextToBoard 用组装好的上下文开黑板（spec §7.2 的 context 同步路径），返回黑板里那个任务。
// 返回的是 taskState：调用方要用 boardMessages 读消息链（后台轮次可能并发读写）。
func contextToBoard(t *testing.T, s *Server, cid string, msgs []llm.Message, task *protocol.Task) *taskState {
	t.Helper()
	tc := protocol.TaskContext{
		ConversationID: cid,
		Messages:       msgs,
		Boss:           "boss-from-context",
		Task:           task,
	}
	out := s.startTask(tc, "boss-from-context", "trace-context-1")
	if got, _ := out.(map[string]any); got["status"] != "accepted" {
		t.Fatalf("context 开任务回执不符: %+v", out)
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

// TestResumeUserFirstChainKeepsFirstMessage：上下文以 user 开头时，reasoner 不得插任务提示，
// 第 0 条用户消息原样留着（任务提示由 memory 基座提供）。
func TestResumeUserFirstChainKeepsFirstMessage(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{
		{Role: "user", Content: "第一条就是用户先开口"},
		{Role: "assistant", Content: "上一轮的助手发言"},
		{Role: "user", Content: "这一轮用户又说了"},
	}
	st := contextToBoard(t, s, "c-user-first", in, &protocol.Task{ID: "task-7", Name: "排查端口"})

	got := boardMessages(st)
	if strings.Join(rolesOf(got), ",") != "user,assistant,user" {
		t.Fatalf("reasoner 不应再插入任务提示，实际 %+v（消息 %+v）", rolesOf(got), got)
	}
	for i := range in {
		assertKept(t, got, i, in[i])
	}
	for _, m := range got {
		if strings.Contains(m.Content, "当前任务：") {
			t.Fatalf("任务提示应由 memory 基座提供，reasoner 不得自己插: %q", m.Content)
		}
	}
}

// TestResumeSystemFirstChainKeepsSystemPromptAtZero：基座第一条 system（任务提示）原样进黑板，
// reasoner 不再在索引 1 插第二条任务提示。
func TestResumeSystemFirstChainKeepsSystemPromptAtZero(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: "现在几点了"},
	}
	st := contextToBoard(t, s, "c-system-first", in, &protocol.Task{ID: "task-8", Name: "查时间"})

	got := boardMessages(st)
	if strings.Join(rolesOf(got), ",") != "system,user" {
		t.Fatalf("reasoner 不应再插入任务提示，实际 %+v", rolesOf(got))
	}
	assertKept(t, got, 0, in[0])
	assertKept(t, got, 1, in[1])
}

// TestResumeWithoutTaskInsertsNothing 钉住门禁：没有任务 id 就不插任务提示，
// 消息链原样进黑板（否则「插入」这件事本身会变成第二条要守的规矩）。
func TestResumeWithoutTaskInsertsNothing(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{
		{Role: "user", Content: "用户先开口"},
		{Role: "assistant", Content: "助手回了一句"},
	}
	st := contextToBoard(t, s, "c-no-task", in, nil)

	got := boardMessages(st)
	if strings.Join(rolesOf(got), ",") != "user,assistant" {
		t.Fatalf("没有任务时不该插入任何消息，实际 %+v", rolesOf(got))
	}
	assertKept(t, got, 0, in[0])
	assertKept(t, got, 1, in[1])
}

// TestContextAdoptsFrameWorkAsChainTrace 说明 context 开任务后这条链的 trace 从哪来：
// 帧头 work 成为任务的 trace，上下文里的 boss 优先、缺省才用帧头 from。
// （tests/ 里的全链断言依赖这个：work 不变，deliver/store/note 才谈得上「同一个工作号」。）
func TestContextAdoptsFrameWorkAsChainTrace(t *testing.T) {
	s := resumeQuietServer(t)
	in := []llm.Message{{Role: "system", Content: systemPrompt}, {Role: "user", Content: "继续"}}
	st := contextToBoard(t, s, "c-trace-keep", in, &protocol.Task{ID: "task-9", Name: "续跑"})

	st.mu.Lock()
	trace, boss, taskID := st.trace, st.boss, st.taskID
	st.mu.Unlock()
	if trace != "trace-context-1" {
		t.Fatalf("任务的 trace 应等于帧头 work，实际 %q", trace)
	}
	if boss != "boss-from-context" {
		t.Fatalf("上下文带 boss 时应优先用它，实际 %q", boss)
	}
	if taskID != "task-9" {
		t.Fatalf("任务 id 应随上下文恢复，实际 %q", taskID)
	}
}
