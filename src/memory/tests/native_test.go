package tests

// memory 的 SPv2.1 原生入口：直接打 /spv2，确认 22 个动作只走原生 method。
// 同时钉住 method 的 Tool 暴露面（与 provides 对齐）与 Input/Output schema。

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// nativeActions 是 server 注册的全部原生动作（顺序无关）。
var nativeActions = []string{
	"context", "store", "history", "conversation_list", "conversation_rename",
	"note", "dig", "lineheads", "prune", "remember", "forget",
	"publish_session", "shared_sessions", "shared_forget",
	"conversation_open", "conversation_archive",
	"task_current", "task_open", "task_finish", "task_resume", "task_rename", "task_list",
}

func TestMemoryNativeSPv2(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	card, err := c.Hello(ctx, spv2.ServiceRef("tester"), spv2.ServiceRef("memory"))
	if err != nil {
		t.Fatal(err)
	}
	wantTools := map[string]bool{"dig": true, "remember": true, "shared_sessions": true}
	for _, name := range nativeActions {
		m, ok := card.Method(name)
		if !ok {
			t.Fatalf("SPv2 卡片缺少原生 method %q", name)
		}
		if m.Tool != wantTools[name] {
			t.Fatalf("method %s 的 Tool=%v，应与 provides 对齐（期望 %v）", name, m.Tool, wantTools[name])
		}
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Fatalf("method %s 缺 Input/Output schema", name)
		}
	}

	call := func(name string, body any) (spv2.Frame, error) {
		f, err := spv2.NewCall("w-native", spv2.ServiceRef("reasoner"), spv2.ServiceRef("memory"), name, body)
		if err != nil {
			t.Fatalf("%s: 构造 call 失败: %v", name, err)
		}
		return c.Call(ctx, f)
	}
	must := func(name string, body any, dst any) spv2.Frame {
		resp, err := call(name, body)
		if err != nil {
			t.Fatalf("%s 原生调用失败: %v", name, err)
		}
		if dst != nil {
			if err := resp.DecodeBody(dst); err != nil {
				t.Fatalf("%s 结果解析失败: %v", name, err)
			}
		}
		return resp
	}

	// 会话 / 记录
	var opened protocol.ConversationResult
	must("conversation_open", protocol.ConversationOpenPayload{ID: "native1", Title: "原生", Entry: "web"}, &opened)
	if opened.Conversation.ID != "native1" {
		t.Fatalf("conversation_open = %+v", opened)
	}
	must("store", protocol.StorePayload{ConversationID: "native1", Boss: "web", Role: "user", Content: "你好"}, nil)
	var hist protocol.HistoryResult
	must("history", protocol.HistoryPayload{ConversationID: "native1"}, &hist)
	if len(hist.Messages) != 1 || hist.Messages[0].Content != "你好" {
		t.Fatalf("history = %+v", hist)
	}
	var list protocol.ConversationListResult
	must("conversation_list", protocol.ConversationListPayload{}, &list)
	if len(list.Conversations) == 0 || list.Conversations[0].ID != "native1" {
		t.Fatalf("conversation_list = %+v", list)
	}
	var renamed protocol.ConversationResult
	must("conversation_rename", protocol.ConversationRenamePayload{ID: "native1", Title: "改过"}, &renamed)
	if renamed.Conversation.Title != "改过" {
		t.Fatalf("conversation_rename = %+v", renamed)
	}
	var archived protocol.ConversationResult
	must("conversation_archive", protocol.ConversationArchivePayload{ID: "native1", Archived: true}, &archived)
	if archived.Conversation.Status != "archived" {
		t.Fatalf("conversation_archive = %+v", archived)
	}

	// 任务
	var current protocol.TaskResult
	must("task_current", protocol.TaskCurrentPayload{ConversationID: "native1"}, &current)
	if current.Task == nil {
		t.Fatal("store 之后应有当前任务")
	}
	taskID := current.Task.ID
	var tasks protocol.TaskListResult
	must("task_list", protocol.TaskListPayload{ConversationID: "native1"}, &tasks)
	if len(tasks.Tasks) != 1 || tasks.Tasks[0].ID != taskID {
		t.Fatalf("task_list = %+v", tasks)
	}
	var renamedTask protocol.TaskResult
	must("task_rename", protocol.TaskRenamePayload{TaskID: taskID, Name: "原生任务"}, &renamedTask)
	if renamedTask.Task == nil || renamedTask.Task.Name != "原生任务" {
		t.Fatalf("task_rename = %+v", renamedTask)
	}
	var finished protocol.TaskResult
	must("task_finish", protocol.TaskFinishPayload{TaskID: taskID}, &finished)
	if finished.Task == nil || finished.Task.Status != "done" {
		t.Fatalf("task_finish = %+v", finished)
	}
	var openedTask protocol.TaskResult
	must("task_open", protocol.TaskOpenPayload{ConversationID: "native2", Name: "恢复我"}, &openedTask)
	if openedTask.Task == nil {
		t.Fatal("task_open 应返回任务")
	}
	var resumed protocol.TaskResumeResult
	must("task_resume", protocol.TaskResumePayload{TaskID: openedTask.Task.ID}, &resumed)
	if resumed.Task.ID != openedTask.Task.ID || len(resumed.Messages) != 0 {
		t.Fatalf("task_resume = %+v", resumed)
	}

	// 长期记忆 / 共享库 / 经验网（无 embedding 服务：note / lineheads 走错误路径）
	var remembered protocol.RememberResult
	must("remember", protocol.RememberPayload{Kind: "身份", Content: "原生记忆", Priority: 1}, &remembered)
	if remembered.ID <= 0 || !remembered.Created {
		t.Fatalf("remember = %+v", remembered)
	}
	must("forget", protocol.ForgetPayload{ID: remembered.ID}, nil)
	var published struct {
		Published bool `msgpack:"published"`
	}
	must("publish_session", protocol.PublishSessionPayload{
		ConversationID: "native1", TaskID: taskID, Title: "原生摘要",
		Goal: "验证迁移", Outcome: "通过", Capabilities: []string{"shell"},
	}, &published)
	if !published.Published {
		t.Fatalf("publish_session = %+v", published)
	}
	must("shared_sessions", protocol.SharedSessionsPayload{Exclude: "native2"}, nil)
	must("shared_forget", protocol.SharedForgetPayload{SessionID: "native1"}, nil)
	must("prune", nil, nil)

	if _, err := call("note", protocol.NotePayload{Content: "一条经验"}); err == nil {
		t.Fatal("note 在缺少 embedding 服务时应报错")
	} else if e := new(spv2.Error); !errors.As(err, &e) || e.Code != spv2.CodeInvalidArgument {
		t.Fatalf("note 错误码不符: %v", err)
	}
	if _, err := call("dig", protocol.DigPayload{}); err == nil {
		t.Fatal("dig 缺少 entry_id/node_id 时应报错")
	}
	if _, err := call("lineheads", protocol.LineHeadsPayload{Query: "原生"}); err == nil {
		t.Fatal("lineheads 在缺少 embedding 服务时应报错")
	}

	// context：原生入口同步返回组装后的消息与 work/task（不再有 resume 回调）。
	ack := must("context", protocol.ContextPayload{
		Conversation: "native3",
		Boss:         "stub-boss",
		Messages:     []protocol.Message{{Role: "system", Content: "sys"}, {Role: "user", Content: "原生上下文"}},
	}, nil)
	var got struct {
		Messages []protocol.Message `msgpack:"messages"`
		Task     protocol.Task      `msgpack:"task"`
	}
	if err := ack.DecodeBody(&got); err != nil {
		t.Fatalf("context 回执解码失败: %v", err)
	}
	if len(got.Messages) == 0 || got.Task.ConversationID != "native3" {
		t.Fatalf("context 回执不符: %+v", got)
	}
}
