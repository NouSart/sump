package tests

// 群聊接入：来源标注（谁、哪个群、多少人看得见、有没有@我）、三档策略，
// 以及"只记录不推理"时写进记忆的那一条。
//
// 这组是"智能体得知道谁在说话"的接缝测试——标注字段错一个，模型就会把群里陌生人的话
// 当成主人的指令，或者把该保密的回复发进群。

import (
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

// ---------- stub memory ----------

type memoryStore struct {
	Payload protocol.StorePayload
	Meta    map[string]any // 解析后的 meta（qq 只在"只记录"模式下带）
}

type stubMemory struct {
	ln        net.Listener
	srv       *http.Server
	mu        sync.Mutex
	stores    []memoryStore
	opens     []protocol.ConversationOpenPayload
	failOpen  bool          // 让 conversation_open 回 ok:false（验证起名失败的处理）
	openDelay time.Duration // 让 conversation_open 慢一点（验证起名不挡消息的路）
}

func startStubMemory(t *testing.T, center *stubCenter) *stubMemory {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sm := &stubMemory{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sm.handleWS)
	sm.srv = &http.Server{Handler: mux}
	go func() { _ = sm.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sm.srv.Close() })

	center.mu.Lock()
	center.cards["memory"] = protocol.ServiceCard{Name: "memory", Addr: sm.URL(), Description: "记忆测试替身"}
	center.mu.Unlock()
	return sm
}

func (sm *stubMemory) URL() string { return "ws://" + sm.ln.Addr().String() + "/ws" }

func (sm *stubMemory) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		if env.Type != protocol.TypeJump {
			continue
		}
		var jp protocol.JumpPayload
		if err := env.DecodePayload(&jp); err != nil {
			continue
		}
		switch jp.Action {
		case "store":
			var p protocol.StorePayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			rec := memoryStore{Payload: p}
			if p.Meta != "" {
				_ = json.Unmarshal([]byte(p.Meta), &rec.Meta)
			}
			sm.mu.Lock()
			sm.stores = append(sm.stores, rec)
			sm.mu.Unlock()
		case "conversation_open":
			var p protocol.ConversationOpenPayload
			_ = protocol.DecodeRaw(jp.Input, &p)
			sm.mu.Lock()
			sm.opens = append(sm.opens, p)
			fail := sm.failOpen
			delay := sm.openDelay
			sm.mu.Unlock()
			if delay > 0 {
				time.Sleep(delay)
			}
			if fail {
				resp, _ := protocol.NewResponse(env, "memory", protocol.ResponsePayload{OK: false, Error: "记忆写不进去"})
				c.send(resp)
				continue
			}
		default:
			resp, _ := protocol.NewResponse(env, "memory", protocol.ResponsePayload{OK: false, Error: "unknown action: " + jp.Action})
			c.send(resp)
			continue
		}
		resp, _ := protocol.NewResponse(env, "memory", protocol.ResponsePayload{OK: true})
		c.send(resp)
	}
}

func (sm *stubMemory) storesLen() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return len(sm.stores)
}

func (sm *stubMemory) opensLen() int {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return len(sm.opens)
}

// waitOpen 等 memory.conversation_open（qq 起会话标题时发）。
func (sm *stubMemory) waitOpen(t *testing.T, timeout time.Duration) protocol.ConversationOpenPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sm.mu.Lock()
		if len(sm.opens) > 0 {
			o := sm.opens[0]
			sm.mu.Unlock()
			return o
		}
		sm.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("等待 memory.conversation_open 超时")
	return protocol.ConversationOpenPayload{}
}

func (sm *stubMemory) waitStore(t *testing.T, timeout time.Duration) memoryStore {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		sm.mu.Lock()
		if len(sm.stores) > 0 {
			s := sm.stores[0]
			sm.mu.Unlock()
			return s
		}
		sm.mu.Unlock()
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("等待 memory.store 超时")
	return memoryStore{}
}

// ---------- 群消息事件 ----------

func groupTextEvent(t *testing.T, groupID, userID int64, text string, mentionSelf bool) map[string]any {
	t.Helper()
	segs := []any{}
	if mentionSelf {
		segs = append(segs, map[string]any{"type": "at", "data": map[string]any{"qq": "2578428650"}})
	}
	segs = append(segs, map[string]any{"type": "text", "data": map[string]any{"text": text}})
	return map[string]any{
		"post_type": "message", "message_type": "group", "sub_type": "normal",
		"user_id": userID, "group_id": groupID, "self_id": 2578428650,
		"message_id": 200, "time": time.Now().Unix(), "raw_message": text, "font": 0,
		"sender":  map[string]any{"user_id": userID, "nickname": "张三", "card": "张三·名片", "role": "member"},
		"message": segs,
	}
}

// senderOf 解析 user_message 载荷里的 sender。
func senderOf(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	s, ok := payload["sender"].(map[string]any)
	if !ok {
		t.Fatalf("user_message 缺少 sender 标注: %+v", payload)
	}
	return s
}

// numOf 把 msgpack 解出来的数字统一成 int（不同宽度解出来类型不同，断言不该关心宽度）。
func numOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int8:
		return int(n)
	case int16:
		return int(n)
	case int32:
		return int(n)
	case int64:
		return int(n)
	case uint64:
		return int(n)
	case float32:
		return int(n)
	case float64:
		return int(n)
	default:
		return -1
	}
}

// countAction 统计 napcat 收到的某个动作次数。
func (n *stubNapCat) countAction(action string) int {
	n.mu.Lock()
	defer n.mu.Unlock()
	count := 0
	for _, m := range n.sent {
		if a, _ := m["action"].(string); a == action {
			count++
		}
	}
	return count
}

// ---------- 用例 ----------

// TestGroupMessageCarriesSender 群消息进任务链时把来源标注带全：
// 群号 / 群名 / 成员数（扩散面）/ 发送者 / 是不是主人 / 有没有@我。
func TestGroupMessageCarriesSender(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "今天几点开会", true))

	req := al.waitRequest(t, 5*time.Second)
	if req.Payload["conversation_id"] != "qq:group:67890" {
		t.Fatalf("群会话标识不符: %+v", req.Payload)
	}
	s := senderOf(t, req.Payload)
	if s["channel"] != "group" || s["id"] != "12345" || s["name"] != "张三·名片" {
		t.Fatalf("发送者标注不符（群名片优先）: %+v", s)
	}
	if s["group_id"] != "67890" {
		t.Fatalf("群号不符: %+v", s)
	}
	if s["owner"] == true {
		t.Fatalf("非主人不能被标成主人: %+v", s)
	}
	if s["mentioned"] != true {
		t.Fatalf("被 @ 应标记提及: %+v", s)
	}
	// 扩散面：群资料来自替身的 get_group_info（群名 + 成员数）。
	// 注意组包时零值字段不出现（omitempty），所以只断言"有值"的情形。
	if s["group_name"] != "测试群" || numOf(s["group_size"]) != 7 {
		t.Fatalf("群资料（扩散面）不符: %+v", s)
	}
}

// TestGroupOwnerMarked 主人本人在群里说话时 owner=true（这是"这条能不能当指令"的关键判据）。
func TestGroupOwnerMarked(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	_ = startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.sendEvent(t, groupTextEvent(t, 67890, 2271917353, "按这个来", false))

	req := al.waitRequest(t, 5*time.Second)
	s := senderOf(t, req.Payload)
	if s["owner"] != true {
		t.Fatalf("主人发言应标 owner=true: %+v", s)
	}
	// 零值字段不出现在线上（omitempty），所以"没被 @"表现为键缺失。
	if s["mentioned"] == true {
		t.Fatalf("没被 @ 不应标记提及: %+v", s)
	}
}

// TestGroupObserveOnlyRecords 观察档：群消息只进上下文（记忆），不开推理轮次。
func TestGroupObserveOnlyRecords(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "observe"}); !rp.OK {
		t.Fatalf("group.mode 应可热改: %+v", rp)
	}
	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "随便聊聊", false))

	saved := sm.waitStore(t, 5*time.Second)
	if saved.Payload.ConversationID != "qq:group:67890" || saved.Payload.Kind != "user" || saved.Payload.Role != "user" {
		t.Fatalf("落库参数不符: %+v", saved.Payload)
	}
	// 正文带来源标注（与推理侧同一份实现），任务名是人话而不是标签。
	if !strings.HasPrefix(saved.Payload.Content, "[群聊 · ") || !strings.Contains(saved.Payload.Content, "随便聊聊") {
		t.Fatalf("正文应带来源标注: %q", saved.Payload.Content)
	}
	if saved.Payload.TaskName != "随便聊聊" {
		t.Fatalf("任务名应是正文而不是标注: %q", saved.Payload.TaskName)
	}
	// meta 里留着结构化事实（界面可以直接画成标签，不必解析正文）；没有附件就不写空数组。
	if _, ok := saved.Meta["sender"].(map[string]any); !ok {
		t.Fatalf("meta 应带结构化 sender: %+v", saved.Meta)
	}
	if _, ok := saved.Meta["images"]; ok {
		t.Fatalf("纯文本消息不该写空的 images 键: %+v", saved.Meta)
	}

	time.Sleep(300 * time.Millisecond)
	if al.count() != 0 {
		t.Fatalf("观察档不应触发推理（收到 %d 条 user_message）", al.count())
	}
}

// TestGroupMentionOnlyReasonsWhenMentioned @ 档：没被点到只记录，被点到才推理。
func TestGroupMentionOnlyReasonsWhenMentioned(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "mention"}); !rp.OK {
		t.Fatalf("group.mode 应可热改: %+v", rp)
	}

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "闲聊一句", false))
	sm.waitStore(t, 5*time.Second)
	time.Sleep(300 * time.Millisecond)
	if al.count() != 0 {
		t.Fatalf("没被 @ 不应推理（收到 %d 条）", al.count())
	}

	nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "机器人你说呢", true))
	req := al.waitRequest(t, 5*time.Second)
	if s := senderOf(t, req.Payload); s["mentioned"] != true {
		t.Fatalf("被 @ 的消息应带提及标记: %+v", s)
	}
}

// TestGroupModeRejectsUnknownValue 非法策略值必须被明确拒绝（不是静默忽略）。
func TestGroupModeRejectsUnknownValue(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "chatty"})
	if rp.OK || !strings.Contains(rp.Error, "observe") {
		t.Fatalf("非法群聊策略应被拒并说明取值范围: %+v", rp)
	}
	if v := qq.SettingValue("group.mode"); v != "judge" {
		t.Fatalf("拒绝后应保持默认值，实际 %q", v)
	}
}

// TestGroupDeliverSendsToGroup deliver 到群会话时真的发到群里（message_type=group）。
func TestGroupDeliverSendsToGroup(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	_ = startStubMemory(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	al.sendDeliver(t, qq.WsURL(), "群里的答复", "qq:group:67890")

	act := nc.waitAction(t, "send_msg", 5*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message_type"] != "group" || params["message"] != "群里的答复" {
		t.Fatalf("send_msg 参数不符: %+v", params)
	}
	if int64(params["group_id"].(float64)) != 67890 {
		t.Fatalf("send_msg 目标群不符: %+v", params)
	}
	if _, has := params["user_id"]; has {
		t.Fatalf("群消息不应带 user_id: %+v", params)
	}
}

// TestGroupQuietImageOnlyMessage 只发了图、没有文字：正文只剩来源标注，
// 任务名必须兜底成 [图片]（否则界面会话树里会出现一串标签）。
func TestGroupQuietImageOnlyMessage(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	startStubImages(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "group.mode", Value: "observe"}); !rp.OK {
		t.Fatalf("group.mode 应可热改: %+v", rp)
	}
	ev := groupTextEvent(t, 67890, 12345, "", false)
	ev["message"] = []any{map[string]any{"type": "image", "data": map[string]any{"url": "http://stub/x.png", "file": "x.png"}}}
	nc.sendEvent(t, ev)

	saved := sm.waitStore(t, 5*time.Second)
	if saved.Payload.TaskName != "[图片]" {
		t.Fatalf("纯图片消息的任务名应兜底成 [图片]，实际 %q", saved.Payload.TaskName)
	}
	if !strings.HasPrefix(saved.Payload.Content, "[群聊 · ") {
		t.Fatalf("正文仍应是来源标注: %q", saved.Payload.Content)
	}
}

// TestGroupInfoIsCached 群资料（群名 + 成员数）只问一次：否则每条消息都要等一次网络往返，
// 网关不支持这个方法时更糟（每条都等满超时）。这里连发三条，断言只问了一次。
func TestGroupInfoIsCached(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	for i := 0; i < 3; i++ {
		nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "第几条", false))
		al.waitRequest(t, 5*time.Second)
	}
	// waitRequest 只等第一条，等三条都被受理再数调用次数。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && al.count() < 3 {
		time.Sleep(50 * time.Millisecond)
	}
	if got := nc.countAction("get_group_info"); got != 1 {
		t.Fatalf("群资料应缓存（只问一次），实际问了 %d 次", got)
	}
}

// TestGroupInfoFailureIsCachedNotRetriedPerMessage 取不到群资料时按"未知"处理并短缓存：
// 不能每条消息都重试一次（网关不支持就变成每条消息卡一次超时）。
func TestGroupInfoFailureIsCachedNotRetriedPerMessage(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.mu.Lock()
	nc.failGroupInfo = true
	nc.mu.Unlock()

	for i := 0; i < 2; i++ {
		nc.sendEvent(t, groupTextEvent(t, 67890, 12345, "还是不知道群名", false))
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && al.count() < 2 {
		time.Sleep(50 * time.Millisecond)
	}
	if al.count() < 2 {
		t.Fatalf("两条消息都应进任务链，实际 %d 条", al.count())
	}
	if got := nc.countAction("get_group_info"); got != 1 {
		t.Fatalf("取不到时也应短缓存（只问一次），实际问了 %d 次", got)
	}
	// 取不到群资料也要能标出扩散面是"未知"，而不是假装小群（标注里 group_size 不出现 = 0）。
	reqs := al.snapshot()
	s := senderOf(t, reqs[0].Payload)
	if s["group_name"] != nil {
		t.Fatalf("群名取不到时不该编一个: %+v", s)
	}
	if _, has := s["group_size"]; has {
		t.Fatalf("人数未知时不该写数字: %+v", s)
	}
}

// TestGroupIgnoresOwnMessages 自己发出去的消息不回灌（否则会自己跟自己聊）。
func TestGroupIgnoresOwnMessages(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	sm := startStubMemory(t, sc)
	_ = startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	nc.sendEvent(t, groupTextEvent(t, 67890, 2578428650, "我自己说的话", false)) // user_id == self_id

	time.Sleep(400 * time.Millisecond)
	if al.count() != 0 || sm.storesLen() != 0 {
		t.Fatalf("自己的消息不该触发任何处理: user_message=%d store=%d", al.count(), sm.storesLen())
	}
}
