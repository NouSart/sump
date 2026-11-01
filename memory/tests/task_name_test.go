package tests

// 任务命名的两个来源：入口服务显式给的 task_name（优先）与从正文第一行推断（兜底）。
//
// 为什么值得一条测试：QQ 的群聊消息正文前面带着来源标注（`[群聊 · 群「xx」 · 12 人可见 · …] 你好`），
// 从正文推会推出一串标签，而任务名要显示在界面的会话树里。入口服务给了名字就必须用它，
// 没给（老调用方 / 门户）则行为不变。

import (
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

// TestStoreUsesExplicitTaskName 验证 store 的两个任务名来源与 recall 的一致性。
func TestStoreUsesExplicitTaskName(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 1) 显式 task_name：正文带标签也不影响任务名。
	labeled := "[群聊 · 群「编程交流」(67890) · 12 人可见 · 张三(12345) · 非主人 · 未@我] 你好"
	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
		ConversationID: "qq:group:67890",
		Role:           "user",
		Kind:           "user",
		Content:        labeled,
		TaskName:       "你好",
	}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
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

	// 2) 没给 task_name：行为与旧版一致（正文第一行）。
	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
		ConversationID: "web:private:owner",
		Role:           "user",
		Content:        "看看现在几点\n附带第二行",
	}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
	rp = caller.call(t, srv.WsURL(), "task_list", protocol.TaskListPayload{ConversationID: "web:private:owner"})
	if !rp.OK {
		t.Fatalf("task_list 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Tasks) != 1 || list.Tasks[0].Name != "看看现在几点" {
		t.Fatalf("兜底取名不符: %+v", list.Tasks)
	}

	// 3) 超长名字压到 24 个字（与 reasoner 的 clampTitle 同口径）。
	long := strings.Repeat("长", 40)
	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
		ConversationID: "qq:private:1",
		Role:           "user",
		Content:        "原始正文",
		TaskName:       long,
	}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
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

// TestRecallHonorsExplicitTaskName 验证 recall 侧也认 task_name：
// store 与 recall 是两条并发路径（Fire 不保证先后），谁先到都不能把标签当任务名。
func TestRecallHonorsExplicitTaskName(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	task := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: "qq:group:99",
		Boss:           "qq",
		TaskName:       "在吗",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "[群聊 · 群 99 · 5 人可见 · 李四(7) · 非主人 · 已@我] 在吗"},
		},
	}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	select {
	case r := <-caller.resumes:
		if r.tc.Task == nil || r.tc.Task.Name != "在吗" {
			t.Fatalf("recall 未采用显式任务名: %+v", r.tc.Task)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到 resume")
	}
}
