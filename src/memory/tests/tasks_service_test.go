package tests

// memory 的任务动作：task_open / task_current / task_list / task_rename / task_finish / task_resume。
// 这组动作是门户“会话 → 任务”与 reasoner 收尾记账的接缝；store 层已有单测，
// 这里走真 WS + 真 SQLite，钉住动作的参数校验、状态流转与返回形状。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// taskResult 解一个单任务回执。
func taskResult(t *testing.T, rp protocol.ResponsePayload) protocol.TaskResult {
	t.Helper()
	var out protocol.TaskResult
	if err := protocol.DecodeRaw(rp.Data, &out); err != nil {
		t.Fatalf("解析任务回执失败: %v", err)
	}
	return out
}

func TestTaskActionsLifecycle(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 空会话：没有当前任务，返回空 Task 而不是报错。
	rp := caller.call(t, srv.WsURL(), "task_current", protocol.TaskCurrentPayload{ConversationID: "task-c1"})
	if !rp.OK {
		t.Fatalf("task_current 失败: %s", rp.Error)
	}
	if got := taskResult(t, rp); got.Task != nil {
		t.Fatalf("空会话不应有当前任务: %+v", got.Task)
	}

	// 新开任务：默认名字与 active 状态。
	rp = caller.call(t, srv.WsURL(), "task_open", protocol.TaskOpenPayload{ConversationID: "task-c1", Name: "写测试"})
	if !rp.OK {
		t.Fatalf("task_open 失败: %s", rp.Error)
	}
	opened := taskResult(t, rp).Task
	if opened == nil || opened.ID == "" || opened.Status != "active" || opened.Name != "写测试" {
		t.Fatalf("新任务字段不符: %+v", opened)
	}
	taskID := opened.ID

	// 当前任务 = 刚开的那条。
	rp = caller.call(t, srv.WsURL(), "task_current", protocol.TaskCurrentPayload{ConversationID: "task-c1"})
	if got := taskResult(t, rp); got.Task == nil || got.Task.ID != taskID {
		t.Fatalf("当前任务不符: %+v", got.Task)
	}

	// 写入两条消息（store 会自动归到当前任务）。
	for _, m := range []protocol.StorePayload{
		{ConversationID: "task-c1", Role: "user", Content: "开始做"},
		{ConversationID: "task-c1", Role: "assistant", Content: "做完了"},
	} {
		if rp := caller.call(t, srv.WsURL(), "store", m); !rp.OK {
			t.Fatalf("store 失败: %s", rp.Error)
		}
	}

	// 改名不改状态。
	rp = caller.call(t, srv.WsURL(), "task_rename", protocol.TaskRenamePayload{TaskID: taskID, Name: "写测试（改名）"})
	if !rp.OK {
		t.Fatalf("task_rename 失败: %s", rp.Error)
	}
	if got := taskResult(t, rp).Task; got == nil || got.Name != "写测试（改名）" || got.Status != "active" {
		t.Fatalf("改名结果不符: %+v", got)
	}

	// 恢复任务：带回该任务自己的记录（两条），且不会把别的任务消息串进来。
	rp = caller.call(t, srv.WsURL(), "task_resume", protocol.TaskResumePayload{TaskID: taskID})
	if !rp.OK {
		t.Fatalf("task_resume 失败: %s", rp.Error)
	}
	var resumed protocol.TaskResumeResult
	if err := protocol.DecodeRaw(rp.Data, &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Task.ID != taskID || resumed.Task.Status != "active" {
		t.Fatalf("恢复的任务不符: %+v", resumed.Task)
	}
	if len(resumed.Messages) != 2 || resumed.Messages[0].Content != "开始做" || resumed.Messages[1].Content != "做完了" {
		t.Fatalf("任务记录不符: %+v", resumed.Messages)
	}

	// 完结（顺手把标题写准）：状态 done、名字生效。
	rp = caller.call(t, srv.WsURL(), "task_finish", protocol.TaskFinishPayload{TaskID: taskID, Name: "写测试（完成）"})
	if !rp.OK {
		t.Fatalf("task_finish 失败: %s", rp.Error)
	}
	done := taskResult(t, rp).Task
	if done == nil || done.Status != "done" || done.Name != "写测试（完成）" {
		t.Fatalf("完结结果不符: %+v", done)
	}

	// 已完结的任务只能看不能继续。
	rp = caller.call(t, srv.WsURL(), "task_resume", protocol.TaskResumePayload{TaskID: taskID})
	if rp.OK || !strings.Contains(rp.Error, "已完结") {
		t.Fatalf("已完结任务恢复应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}

	// 完结后可以再开一个新任务；列表最近活跃在前。
	rp = caller.call(t, srv.WsURL(), "task_open", protocol.TaskOpenPayload{ConversationID: "task-c1", Name: "下一件事"})
	if !rp.OK {
		t.Fatalf("第二个 task_open 失败: %s", rp.Error)
	}
	next := taskResult(t, rp).Task

	// 当前任务跟着最近一次 open 走。
	rp = caller.call(t, srv.WsURL(), "task_current", protocol.TaskCurrentPayload{ConversationID: "task-c1"})
	if got := taskResult(t, rp); got.Task == nil || got.Task.ID != next.ID {
		t.Fatalf("当前任务应切到新任务: %+v", got.Task)
	}

	rp = caller.call(t, srv.WsURL(), "task_list", protocol.TaskListPayload{ConversationID: "task-c1"})
	if !rp.OK {
		t.Fatalf("task_list 失败: %s", rp.Error)
	}
	var list protocol.TaskListResult
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 2 || list.Tasks[0].ID != next.ID || list.Tasks[1].ID != taskID {
		t.Fatalf("任务列表应按最近活跃排序: %+v", list.Tasks)
	}

	// limit 生效。
	rp = caller.call(t, srv.WsURL(), "task_list", protocol.TaskListPayload{ConversationID: "task-c1", Limit: 1})
	if !rp.OK {
		t.Fatalf("task_list(limit) 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].ID != next.ID {
		t.Fatalf("limit 应只取最近一条: %+v", list.Tasks)
	}

	// 没写名字的任务用默认名，而不是空串。
	rp = caller.call(t, srv.WsURL(), "task_open", protocol.TaskOpenPayload{ConversationID: "task-c2"})
	if got := taskResult(t, rp).Task; got == nil || got.Name != "未命名任务" {
		t.Fatalf("空名字应回落默认名: %+v", got)
	}
}

func TestTaskActionsValidateInput(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	cases := []struct {
		name   string
		action string
		input  any
		want   string
	}{
		{"finish_without_id", "task_finish", protocol.TaskFinishPayload{}, "task_id 必填"},
		{"resume_without_id", "task_resume", protocol.TaskResumePayload{}, "task_id 必填"},
		{"rename_without_id", "task_rename", protocol.TaskRenamePayload{}, "task_id 必填"},
		{"resume_unknown", "task_resume", protocol.TaskResumePayload{TaskID: "no-such-task"}, "任务不存在"},
		{"finish_unknown", "task_finish", protocol.TaskFinishPayload{TaskID: "no-such-task"}, "完结任务失败"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := caller.call(t, srv.WsURL(), tc.action, tc.input)
			if rp.OK || !strings.Contains(rp.Error, tc.want) {
				t.Fatalf("%s 应被拒绝并说明原因 %q: ok=%v err=%q", tc.action, tc.want, rp.OK, rp.Error)
			}
		})
	}

	// 空会话的列表是空数组（不是 null），门户可直接渲染。
	rp := caller.call(t, srv.WsURL(), "task_list", protocol.TaskListPayload{ConversationID: "empty"})
	if !rp.OK {
		t.Fatalf("task_list 失败: %s", rp.Error)
	}
	var list protocol.TaskListResult
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 0 {
		t.Fatalf("空会话不应有任务: %+v", list.Tasks)
	}
}
