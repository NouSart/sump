package tests

// 会话标题：门户侧栏里 "qq:group:67890" 这种号码本身没法看，qq 要在第一次见到会话时
// 用 memory 的 conversation_open 起一个人话标题（"QQ 群「测试群」" / "QQ 私聊 · 张三"）。
//
// 这里盯三件事：标题拼得对、不知道名字时不编名字、每个会话只试一次（不为锦上添花
// 每条消息都多打一次跨服务调用）。

import (
	"testing"
	"time"
)

// TestGroupConversationTitled 群会话按群名起标题。
func TestGroupConversationTitled(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	_ = startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "今天几点开会", true))

	open := sm.waitOpen(t, 5*time.Second)
	if open.ID != "qq:group:67890" {
		t.Fatalf("起名的会话号不符: %+v", open)
	}
	if open.Title != "QQ 群「测试群」" {
		t.Fatalf("群标题应是群名：%q", open.Title)
	}
}

// TestPrivateConversationTitled 私聊按对方的名字起标题；没有名字就用 QQ 号顶着。
func TestPrivateConversationTitled(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	_ = startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	ev := privateTextEvent(2271917353, "在吗")
	ev["sender"] = map[string]any{"user_id": 2271917353, "nickname": "主人", "card": ""}
	nc.sendEvent(t, ev)

	open := sm.waitOpen(t, 5*time.Second)
	if open.ID != "qq:private:2271917353" {
		t.Fatalf("起名的会话号不符: %+v", open)
	}
	if open.Title != "QQ 私聊 · 主人" {
		t.Fatalf("私聊标题应是对方称呼：%q", open.Title)
	}
}

// TestPrivateConversationTitledWithIDFallback 昵称拿不到时标题退到 QQ 号——
// 宁可显示号码，也不要造一个"未知好友"。
func TestPrivateConversationTitledWithIDFallback(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	_ = startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.sendEvent(t, privateTextEvent(2271917353, "在吗"))

	open := sm.waitOpen(t, 5*time.Second)
	if open.Title != "QQ 私聊 · 2271917353" {
		t.Fatalf("没名字时应退到 QQ 号：%q", open.Title)
	}
}

// TestGroupConversationNotTitledWithoutName 取不到群名时不起名：
// 没名字就用会话号兜底（门户侧栏会显示 "QQ 群 67890"），别编一个 "QQ 群「」"。
func TestGroupConversationNotTitledWithoutName(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.mu.Lock()
	nc.failGroupInfo = true
	nc.mu.Unlock()

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "群名取不到", true))
	al.waitRequest(t, 5*time.Second)

	time.Sleep(300 * time.Millisecond)
	if n := sm.opensLen(); n != 0 {
		t.Fatalf("群名未知时不该起名，实际调了 %d 次", n)
	}
}

// TestConversationTitleFailureNotRetried 起名失败也不重试：起名是锦上添花，
// 记忆服务抖动时不该让每条群消息都先等一次跨服务调用（群消息热路径）。
func TestConversationTitleFailureNotRetried(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	sm.mu.Lock()
	sm.failOpen = true
	sm.mu.Unlock()

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第一句", true))
	sm.waitOpen(t, 5*time.Second)
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第二句", true))
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && al.count() < 2 {
		time.Sleep(50 * time.Millisecond)
	}

	if n := sm.opensLen(); n != 1 {
		t.Fatalf("起名失败后不该再试，实际调了 %d 次", n)
	}
	if al.count() < 2 {
		t.Fatalf("起名失败不该挡住消息进任务链，实际 %d 条", al.count())
	}
}

// TestConversationTitleDoesNotBlockMessage 起名慢（记忆服务抖）不能拖住消息本身：
// 标题是给人看的锦上添花，聊天该先走。
func TestConversationTitleDoesNotBlockMessage(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	sm.mu.Lock()
	sm.openDelay = 3 * time.Second
	sm.mu.Unlock()

	start := time.Now()
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "先说正事", true))
	al.waitRequest(t, 5*time.Second)
	if waited := time.Since(start); waited > time.Second {
		t.Fatalf("起名慢不该拖住消息进任务链，实际等了 %v", waited)
	}
}

// TestConversationTitledOnce 同一个会话的第二条消息不再起名（起名是锦上添花，不是每条消息的活）。
func TestConversationTitledOnce(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	_ = startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第一句", true))
	sm.waitOpen(t, 5*time.Second)
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第二句", true))

	time.Sleep(500 * time.Millisecond)
	if n := sm.opensLen(); n != 1 {
		t.Fatalf("同一会话只该起名一次，实际调了 %d 次", n)
	}
}
