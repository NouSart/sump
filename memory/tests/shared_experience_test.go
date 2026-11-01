// 共享经验库在**记忆服务层**的行为：
//   - 别的会话发布了摘要 → 这一轮 recall 能看到（但看不到它的对话原文）；
//   - private 的任务不进目录；
//   - share.sessions_limit = 0 时本节消失；
//   - 删除共享物后立刻消失；
//   - 共享库缺席时 recall 照常成功（本节省略）。
package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/compose"
	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
)

// recallBlock 跑一轮 recall 并把注入的记忆块取出来（没有就返回空）。
func recallMemoryBlock(t *testing.T, caller *stubCaller, memoryURL, cid, userText string) string {
	t.Helper()
	p := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: cid,
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: userText},
		},
	}}
	if rp := caller.recallUntilReady(t, memoryURL, p); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	select {
	case r := <-caller.resumes:
		for _, m := range r.tc.Messages {
			if m.Role == "system" && strings.HasPrefix(m.Content, compose.MemoryPrefix) {
				return m.Content
			}
		}
		return ""
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到 resume")
		return ""
	}
}

// publish 以 reasoner 的身份发布会话摘要。
func publish(t *testing.T, caller *stubCaller, memoryURL string, p protocol.PublishSessionPayload) protocol.ResponsePayload {
	t.Helper()
	rp := caller.call(t, memoryURL, "publish_session", p)
	if !rp.OK {
		t.Fatalf("publish_session 失败: %s", rp.Error)
	}
	return rp
}

// TestRecallInjectsOtherSessions 是这一层的核心断言：
// A 会话干过的事，B 会话看得到摘要，看不到 A 的消息原文与会话 id。
func TestRecallInjectsOtherSessions(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// A 会话：存一条"原文"，再发布摘要。
	const secret = "机密原文：数据库口令是 hunter2"
	if rp := caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
		ConversationID: "qq:private:42", Role: "user", Content: secret,
	}); !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: "qq:private:42",
		TaskID:         "task-a",
		Title:          "排查 embedding",
		Goal:           "排查 embedding 缺模型的报错",
		Outcome:        "已定位到模型文件缺失",
		Capabilities:   []string{"shell", "files"},
	})

	// B 会话：召回。
	block := recallMemoryBlock(t, caller, srv.WsURL(), "web:private:owner", "继续改门户")

	if !strings.Contains(block, "（其他会话）") {
		t.Fatalf("没有注入其他会话一节: %q", block)
	}
	if !strings.Contains(block, "QQ「排查 embedding」") {
		t.Fatalf("摘要缺失: %q", block)
	}
	if !strings.Contains(block, "已定位到模型文件缺失") {
		t.Fatalf("做到哪了缺失: %q", block)
	}
	// 边界：原文与会话 id 都不该出现在别的会话的上下文里。
	if strings.Contains(block, secret) {
		t.Fatalf("泄漏了别的会话的消息原文: %q", block)
	}
	if strings.Contains(block, "qq:private:42") {
		t.Fatalf("泄漏了别的会话 id: %q", block)
	}
}

// TestPublishPrivateNotListed 验证 private 任务连会话行都不写。
func TestPublishPrivateNotListed(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: "qq:private:7", TaskID: "task-p",
		Goal: "不该被别的会话看到", Private: true,
	})

	block := recallMemoryBlock(t, caller, srv.WsURL(), "web:private:owner", "随便问问")
	if strings.Contains(block, "（其他会话）") {
		t.Fatalf("private 任务不该进目录: %q", block)
	}
}

// TestShareSessionsLimitZero 验证预算设为 0 = 不带本节。
func TestShareSessionsLimitZero(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: "qq:private:9", TaskID: "task-1", Goal: "别的会话在做的事",
	})
	if rp := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{
		Key: "share.sessions_limit", Value: "0", Default: "5", Overridden: true,
	}); !rp.OK {
		t.Fatalf("configure 失败: %s", rp.Error)
	}
	block := recallMemoryBlock(t, caller, srv.WsURL(), "web:private:owner", "再问一次")
	if strings.Contains(block, "（其他会话）") {
		t.Fatalf("share.sessions_limit=0 时不该注入: %q", block)
	}
}

// TestSharedForgetRemoves 验证删共享物后本轮立刻看不到。
func TestSharedForgetRemoves(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: "qq:private:11", TaskID: "task-x", Goal: "这条马上要被删掉",
	})
	if rp := caller.call(t, srv.WsURL(), "shared_forget", protocol.SharedForgetPayload{SessionID: "qq:private:11"}); !rp.OK {
		t.Fatalf("shared_forget 失败: %s", rp.Error)
	}
	block := recallMemoryBlock(t, caller, srv.WsURL(), "web:private:owner", "删掉了吗")
	if strings.Contains(block, "这条马上要被删掉") {
		t.Fatalf("删了还在: %q", block)
	}
}

// TestRecallWithoutSharedStore 验证共享库缺席时 recall 照常完成（本节省略，不阻断链）。
func TestRecallWithoutSharedStore(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12, func(cfg *server.Config, _ *store.Store) {
		cfg.Shared = nil
	})
	caller := startStubCaller(t, sc.URL())

	block := recallMemoryBlock(t, caller, srv.WsURL(), "web:private:owner", "没有共享库也要能跑")
	if strings.Contains(block, "（其他会话）") {
		t.Fatalf("没有共享库时不该有本节: %q", block)
	}
	if rp := caller.call(t, srv.WsURL(), "publish_session", protocol.PublishSessionPayload{
		ConversationID: "web:private:owner", TaskID: "t", Goal: "x",
	}); !rp.OK {
		t.Fatalf("共享库缺席时 publish 应是无操作而不是错误: %s", rp.Error)
	}
}

// TestRecallExcludesOwnSession 验证「目录永远不含自己」这条硬规则。
//
// 为什么必须单独钉：如果只看"别人的摘要进来了"，把 exclude 写错成空串也照样能通过——
// 那时自己的摘要会跟着混进来，模型对着自己的记录复述，看起来还挺合理。
func TestRecallExcludesOwnSession(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	const self = "web:private:owner"
	// 自己发布过摘要（另一个任务），同时别的会话也有一条。
	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: self, TaskID: "task-self", Goal: "自己上一个任务在做的事",
	})
	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: "qq:private:31", TaskID: "task-other", Goal: "别的会话在做的事",
	})

	block := recallMemoryBlock(t, caller, srv.WsURL(), self, "接着干")
	if !strings.Contains(block, "别的会话在做的事") {
		t.Fatalf("别的会话的摘要应出现: %q", block)
	}
	if strings.Contains(block, "自己上一个任务在做的事") {
		t.Fatalf("「其他会话」一节里不该出现自己的摘要: %q", block)
	}
}

// TestSharedSessionsTool 验证工具动作返回目录（并排除自己）。
func TestSharedSessionsTool(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	publish(t, caller, srv.WsURL(), protocol.PublishSessionPayload{
		ConversationID: "qq:private:21", TaskID: "task-1", Goal: "做了一件别的事",
	})
	rp := caller.call(t, srv.WsURL(), "shared_sessions", protocol.SharedSessionsPayload{Exclude: "web:private:owner"})
	if !rp.OK {
		t.Fatalf("shared_sessions 失败: %s", rp.Error)
	}
	var res protocol.SharedSessionsResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Sessions) != 1 || res.Sessions[0].Entry != "QQ" {
		t.Fatalf("目录 = %+v", res.Sessions)
	}
	if len(res.Sessions[0].Tasks) != 1 || res.Sessions[0].Tasks[0].Goal != "做了一件别的事" {
		t.Fatalf("任务摘要 = %+v", res.Sessions[0].Tasks)
	}
}
