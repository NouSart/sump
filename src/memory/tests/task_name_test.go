package tests

// 任务命名的来源：work_open 显式给的 task_name（优先）与 memory 的兜底（空名 → 未命名任务）。
//
// 为什么值得一条测试：QQ 的群聊消息正文前面带着来源标注（`[群聊 · 群「xx」 · 12 人可见 · …] 你好`），
// 从正文推会推出一串标签，而任务名要显示在界面的会话树里。入口服务给了名字就必须用它；
// 名字过长时截到 24 字（与协议包 ClampTaskName 同一把尺子）。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// TestWorkOpenUsesExplicitTaskName 验证 task_name 优先与超长截断。
func TestWorkOpenUsesExplicitTaskName(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 1) 显式 task_name：正文带标签也不影响任务名。
	labeled := "[群聊 · 群「编程交流」(67890) · 12 人可见 · 张三(12345) · 非主人 · 未@我] 你好"
	opened := caller.openWork(t, srv.WsURL(), "w-name", "qq:group:67890", "你好")
	caller.storeV2(t, srv.WsURL(), "w-name", "qq:group:67890", opened.TaskID, "user", labeled, "reasoner:name-1")

	rp := caller.call(t, srv.WsURL(), "task_list", protocol.TaskListPayload{ConversationID: "qq:group:67890"})
	if !rp.OK {
		t.Fatalf("task_list 失败: %s", rp.Error)
	}
	var list protocol.TaskListResult
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("应有一个任务: %+v", list.Tasks)
	}
	if list.Tasks[0].Name != "你好" {
		t.Fatalf("显式 task_name 未生效（取到 %q）", list.Tasks[0].Name)
	}

	// 2) 超长名字压到 24 个字（与 reasoner 的 clampTitle 同口径）。
	long := strings.Repeat("长", 40)
	caller.openWork(t, srv.WsURL(), "w-long", "qq:private:1", long)
	rp = caller.call(t, srv.WsURL(), "task_list", protocol.TaskListPayload{ConversationID: "qq:private:1"})
	if !rp.OK {
		t.Fatalf("task_list 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 {
		t.Fatalf("应有一个任务: %+v", list.Tasks)
	}
	if got := len([]rune(list.Tasks[0].Name)); got != 25 { // 24 字 + 省略号
		t.Fatalf("超长任务名应压到 24 字 + …，实际 %d 字: %q", got, list.Tasks[0].Name)
	}
}

// TestContextHonorsExplicitTaskName 验证 context 的 task 投影就是 work_open 建的任务，
// 正文标签不会变成任务名。
func TestContextHonorsExplicitTaskName(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	opened := caller.openWork(t, srv.WsURL(), "trace-qq99", "qq:group:99", "在吗")
	msg := caller.storeV2(t, srv.WsURL(), "trace-qq99", "qq:group:99", opened.TaskID, "user",
		"[群聊 · 群 99 · 5 人可见 · 李四(7) · 非主人 · 已@我] 在吗", "reasoner:qq99")
	got := caller.contextFor(t, srv.WsURL(), "trace-qq99", protocol.Trigger{Kind: "user", Item: msg.ItemID})
	if got.Task.Name != "在吗" {
		t.Fatalf("context 未采用显式任务名: %+v", got.Task)
	}
}
