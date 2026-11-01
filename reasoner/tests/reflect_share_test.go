package tests

// 复盘 → 共享经验库的契约（服务层，真 WS）：
//
//   - 复盘顺带产出任务摘要并发给记忆服务（publish_session），零额外模型调用；
//   - share.on=false 时只写经验、不发摘要；
//   - reflect.on=false 时摘要也不发（没有复盘就没有摘要——文档里写明的依赖）；
//   - 摘要里的 private 原样带到载荷（是否进目录由记忆服务决定）。
//
// 这些用例盯的是"边界"：摘要必须是提炼物，且**关得掉**。关不掉就等于偷偷外泄。

import (
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/server"
)

const digestJSON = `{"digest":{"goal":"修序号问题","outcome":"已改完并跑过测试","private":false},
	"notes":[{"content":"并发写要带会话内单调序号","kind":"坑"}]}`

// publishEventually 等记忆替身收到发布（复盘在 goroutine 里，必须轮询）。
func publishEventually(t *testing.T, m *stubMemory, want int, timeout time.Duration) []protocol.PublishSessionPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := m.Publishes(); len(got) >= want {
			return got
		}
		time.Sleep(50 * time.Millisecond)
	}
	return m.Publishes()
}

// TestReflectPublishesDigest 验证摘要真的发出去了，且带齐对话 / 任务 / 标题。
func TestReflectPublishesDigest(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{notes: digestJSON}
	s, boss, mem := startReflectEnv(t, sc, client, func(c *server.Config) { c.Reflect = true; c.Share = true })
	mem.setTask(&protocol.Task{ID: "task-reflect", ConversationID: "default", Name: "修序号", Status: "active"})

	sendUser(t, s, "把序号问题修一下")
	waitDelivery(t, boss, "任务答复")

	got := publishEventually(t, mem, 1, 10*time.Second)
	if len(got) != 1 {
		t.Fatalf("应发布 1 条摘要，实际 %d: %+v", len(got), got)
	}
	p := got[0]
	if p.ConversationID != "default" || p.TaskID != "task-reflect" {
		t.Fatalf("摘要归属不对: %+v", p)
	}
	if p.Title != "修序号" || p.Goal != "修序号问题" || p.Outcome != "已改完并跑过测试" {
		t.Fatalf("摘要内容不对: %+v", p)
	}
	if p.Private {
		t.Fatalf("模型没说 private，不该自己加上: %+v", p)
	}
}

// TestReflectPrivateDigest 验证模型标的 private 原样带到载荷。
func TestReflectPrivateDigest(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{notes: `{"digest":{"goal":"处理私人事务","outcome":"完成","private":true},"notes":[]}`}
	s, boss, mem := startReflectEnv(t, sc, client, func(c *server.Config) { c.Reflect = true; c.Share = true })
	mem.setTask(&protocol.Task{ID: "task-p", ConversationID: "default", Name: "私事", Status: "active"})

	sendUser(t, s, "帮我处理一件私事")
	waitDelivery(t, boss, "任务答复")

	got := publishEventually(t, mem, 1, 10*time.Second)
	if len(got) != 1 || !got[0].Private {
		t.Fatalf("private 应原样带到载荷: %+v", got)
	}
}

// TestShareOffSkipsPublish 验证 share.on=false 时只写经验，不发摘要。
func TestShareOffSkipsPublish(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{notes: digestJSON}
	s, boss, mem := startReflectEnv(t, sc, client, func(c *server.Config) { c.Reflect = true; c.Share = false })
	mem.setTask(&protocol.Task{ID: "task-x", ConversationID: "default", Name: "随便", Status: "active"})

	sendUser(t, s, "做点事")
	waitDelivery(t, boss, "任务答复")

	if notes := noteEventually(t, mem, 1, 10*time.Second); len(notes) != 1 {
		t.Fatalf("经验照常写: %+v", notes)
	}
	// 复盘是异步的：给它一点时间，再断言"确实没发"。
	time.Sleep(300 * time.Millisecond)
	if got := mem.Publishes(); len(got) != 0 {
		t.Fatalf("share.on=false 不该发布摘要: %+v", got)
	}
}

// TestReflectOffSkipsPublish 验证没有复盘就没有摘要（写进文档的依赖关系）。
func TestReflectOffSkipsPublish(t *testing.T) {
	sc := startStubCenter(t)
	client := &scriptedLLM{notes: digestJSON}
	s, boss, mem := startReflectEnv(t, sc, client, func(c *server.Config) { c.Reflect = false; c.Share = true })
	mem.setTask(&protocol.Task{ID: "task-y", ConversationID: "default", Name: "随便", Status: "active"})

	sendUser(t, s, "做点事")
	waitDelivery(t, boss, "任务答复")

	time.Sleep(300 * time.Millisecond)
	if client.Reflects() != 0 {
		t.Fatalf("reflect.on=false 不该复盘，实际 %d 次", client.Reflects())
	}
	if got := mem.Publishes(); len(got) != 0 {
		t.Fatalf("没有复盘就不该有摘要: %+v", got)
	}
}
