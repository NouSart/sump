package tests

// v2 入口的跨 work / 匿名红队验收（R-023 P4，3 进程：session + experience + facade）。
//
// 来源 af6cfc8:src/memory/tests/redteam_v2_test.go 的四条红线中与 v2 入口相关的用例，
// 按新架构改写：
//   - work 由 facade 的 context → session.context_base 真实登记（owner=部署默认人，
//     agent=原帧 from），不再直接读写会话库；
//   - 所有调用都经过 facade，从而顺带验证门面用 CallWithRef(from=原帧 From) 转发；
//   - 拒绝必须等于零副作用：
//       store           → 目标会话查不到内容，同一 client_item_id 仍能被本主正常写入；
//       context_v2      → 目标触发仍是 pending（trigger_complete 回 conflict）；
//       trigger_complete→ 目标触发仍能由本主完成（不是 conflict）；
//       call_open       → 目标 work 的常驻目录行数不变；
//       call_control    → 目标 call 两维不变，且同一 control_id 仍能被本主正常使用；
//   - 匿名 from.id 为空：帧校验就会拒绝（client 构造器不允许发出），这里用裸 WS 手写帧，
//     断言拒绝 + 零写入。授权原语层的 permission_denied 由 session/authorize_test.go 钉住。

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestRedTeamV2CrossWorkAuthorization(t *testing.T) {
	memURL := startRedTeamV2Stack(t)

	const (
		alice = "w-red-alice"
		bob   = "w-red-bob"
	)
	// 两条 work：agent 分别是 reasoner-a / reasoner-b；owner 都是部署默认人。
	seedRedTeamWork(t, memURL, alice, "reasoner-a", "conv-red-alice")
	seedRedTeamWork(t, memURL, bob, "reasoner-b", "conv-red-bob")

	t.Run("跨 work store 拒绝且零写入", func(t *testing.T) {
		const content = "红队：冒充 bob 写入"
		key := "ci-red-cross-" + spv2.NewID()
		_, err := redTeamStore(t, memURL, bob, "reasoner-a", content, key)
		expectRedTeamCode(t, err, spv2.CodePermissionDenied, "跨 work store")

		if redTeamHistoryContains(t, memURL, "conv-red-bob", content) {
			t.Fatal("越权 store 不应落消息")
		}
		// 同一 client_item_id 仍能被本主写入：证明拒绝路径没留下幂等回执占位。
		res, err := redTeamStore(t, memURL, bob, "reasoner-b", content, key)
		if err != nil {
			t.Fatalf("本主用同一 client_item_id 写入应成功（拒绝路径不应留回执）: %v", err)
		}
		if res.ItemID == "" || res.Deduped {
			t.Fatalf("本主首次写入应是非去重的新条目: %+v", res)
		}
	})

	t.Run("匿名 store 被拒且零写入", func(t *testing.T) {
		const content = "红队：匿名写入"
		key := "ci-red-anon-" + spv2.NewID()
		err := redTeamAnonymousStore(t, memURL, bob, content, key)
		if err == nil {
			t.Fatal("匿名 store 必须被拒绝")
		}
		// 匿名帧在 handler 之前就被帧校验拦住：invalid_argument；若未来帧校验放宽，
		// 这里仍然接受授权层的 permission_denied（都表示拒绝）。
		if !spv2.IsCode(err, spv2.CodeInvalidArgument) && !spv2.IsCode(err, spv2.CodePermissionDenied) {
			t.Fatalf("匿名 store 的错误码应显式（invalid_argument / permission_denied），实际: %v", err)
		}
		if redTeamHistoryContains(t, memURL, "conv-red-bob", content) {
			t.Fatal("匿名 store 不应落消息")
		}
		if _, err := redTeamStore(t, memURL, bob, "reasoner-b", content, key); err != nil {
			t.Fatalf("匿名拒绝后本主用同一 client_item_id 写入应成功: %v", err)
		}
	})

	t.Run("跨 work context_v2 拒绝且不产出快照", func(t *testing.T) {
		item := redTeamStoreMessage(t, memURL, bob, "reasoner-b", "红队：bob 尚未组装的触发")
		_, err := redTeamCall(t, memURL, alice, "reasoner-a", "context_v2", protocol.ContextRequest{
			Work: bob, Trigger: protocol.Trigger{Kind: "user", Item: item},
		})
		expectRedTeamCode(t, err, spv2.CodePermissionDenied, "跨 work context_v2")

		// 仍是 pending：没组装过就不能标记完成（若越权读取产出快照，这里会成功）。
		_, err = redTeamCall(t, memURL, bob, "reasoner-b", "trigger_complete", protocol.TriggerCompletePayload{
			Work: bob, Trigger: protocol.Trigger{Kind: "user", Item: item},
		})
		expectRedTeamCode(t, err, spv2.CodeConflict, "越权读取后 bob 的触发仍应 pending")
	})

	t.Run("跨 work trigger_complete 拒绝且不改状态", func(t *testing.T) {
		item := redTeamStoreMessage(t, memURL, bob, "reasoner-b", "红队：bob 尚未完成的触发")
		_, err := redTeamCall(t, memURL, alice, "reasoner-a", "trigger_complete", protocol.TriggerCompletePayload{
			Work: bob, Trigger: protocol.Trigger{Kind: "user", Item: item},
		})
		expectRedTeamCode(t, err, spv2.CodePermissionDenied, "跨 work trigger_complete")

		// 攻击者若写进了 completed，本主这次会成功；conflict 才说明状态仍是 pending。
		_, err = redTeamCall(t, memURL, bob, "reasoner-b", "trigger_complete", protocol.TriggerCompletePayload{
			Work: bob, Trigger: protocol.Trigger{Kind: "user", Item: item},
		})
		expectRedTeamCode(t, err, spv2.CodeConflict, "越权完成被拒后，bob 的触发仍应 pending")
	})

	t.Run("跨 work call_open 拒绝且零落库", func(t *testing.T) {
		before := redTeamResidentCount(t, memURL, bob, "reasoner-b")
		_, err := redTeamCall(t, memURL, alice, "reasoner-a", "call_open", protocol.CallOpenPayload{
			Work: bob, Service: "shell", Action: "run", Mode: "resident", Pausable: true,
		})
		expectRedTeamCode(t, err, spv2.CodePermissionDenied, "跨 work call_open")
		if after := redTeamResidentCount(t, memURL, bob, "reasoner-b"); after != before {
			t.Fatalf("越权 call_open 不应落调用：常驻行数 %d → %d", before, after)
		}

		// 本主自己 open 成功（证明门面转发保留了原帧 from，否则这里也会越权）。
		if _, err := redTeamCall(t, memURL, bob, "reasoner-b", "call_open", protocol.CallOpenPayload{
			Work: bob, Service: "shell", Action: "run", Mode: "resident", Pausable: true,
		}); err != nil {
			t.Fatalf("bob 自己 call_open 应成功: %v", err)
		}
	})

	t.Run("跨 work cancel 拒绝且状态不变", func(t *testing.T) {
		var opened protocol.CallOpenResult
		body, err := redTeamCall(t, memURL, bob, "reasoner-b", "call_open", protocol.CallOpenPayload{
			Work: bob, Service: "shell", Action: "run", Mode: "once", Pausable: true,
		})
		if err != nil {
			t.Fatalf("bob 自己 call_open 应成功: %v", err)
		}
		if err := protocol.DecodeRaw(body, &opened); err != nil {
			t.Fatal(err)
		}
		const controlID = "ctl-red-cross"
		_, err = redTeamCall(t, memURL, alice, "reasoner-a", "call_control", protocol.CallControlPayload{
			ID: opened.CallID, ControlID: controlID, Op: "cancel",
		})
		expectRedTeamCode(t, err, spv2.CodePermissionDenied, "跨 work cancel")

		// 越权被拒后目标 call 的两维必须原样。
		status, err := redTeamStatus(t, memURL, bob, "reasoner-b", opened.CallID)
		if err != nil {
			t.Fatalf("bob 自己 call_status 应成功: %v", err)
		}
		if len(status.Items) != 1 || status.Items[0].Observe != "waiting" || status.Items[0].Exec != "created" {
			t.Fatalf("越权 cancel 不应改动目标调用: %+v", status.Items)
		}
		// 同一 control_id 仍能被本主使用：证明拒绝路径没留下控制记录。
		if _, err := redTeamCall(t, memURL, bob, "reasoner-b", "call_control", protocol.CallControlPayload{
			ID: opened.CallID, ControlID: controlID, Op: "cancel",
		}); err != nil {
			t.Fatalf("越权拒绝后 bob 用同一 control_id 取消应成功: %v", err)
		}
	})

	t.Run("work 不存在时四个入口都显式 not_found", func(t *testing.T) {
		const ghost = "w-red-ghost"
		cases := []struct {
			name   string
			method string
			body   any
		}{
			{"store", "store", protocol.StorePayload{
				ConversationID: "conv-red-ghost", Work: ghost, Role: "user",
				Content: "写幽灵 work", ClientItemID: "ci-red-ghost",
			}},
			{"context_v2", "context_v2", protocol.ContextRequest{
				Work: ghost, Trigger: protocol.Trigger{Kind: "user", Item: "1"},
			}},
			{"trigger_complete", "trigger_complete", protocol.TriggerCompletePayload{
				Work: ghost, Trigger: protocol.Trigger{Kind: "user", Item: "1"},
			}},
			{"call_open", "call_open", protocol.CallOpenPayload{Work: ghost, Service: "shell"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				_, err := redTeamCall(t, memURL, ghost, "reasoner-a", tc.method, tc.body)
				expectRedTeamCode(t, err, spv2.CodeNotFound, tc.name+" 的 work 不存在")
			})
		}
	})
}

// ---------- 三进程环境 ----------

// startRedTeamV2Stack 起 设置中心 + memory 三进程（session / experience / facade），返回门面 URL。
func startRedTeamV2Stack(t *testing.T) string {
	t.Helper()
	centerBin := buildService(t, "github.com/NouSart/sump/settings-center")
	centerPort := freePort(t)
	centerURL := spv2URL(centerPort)
	startProc(t, centerBin, "-addr", fmt.Sprintf("127.0.0.1:%d", centerPort))
	waitWSReady(t, centerURL, 60*time.Second)

	dir := t.TempDir()
	return startMemoryStackAt(t, centerURL, nil,
		filepath.Join(dir, "memory.db"), filepath.Join(dir, "experience.db"))
}

// seedRedTeamWork 用 context（facade → session.context_base）登记一条 work：
// owner=部署默认人，agent=from，entry=from；同时建好会话与任务。
func seedRedTeamWork(t *testing.T, memURL, work, from, conversation string) {
	t.Helper()
	_, err := redTeamCall(t, memURL, work, from, "context", protocol.ContextPayload{
		Conversation: conversation,
		Messages:     []protocol.Message{{Role: "user", Content: "红队 v2 建 work"}},
	})
	if err != nil {
		t.Fatalf("登记 work %s 失败: %v", work, err)
	}
}

// ---------- 原生调用辅助 ----------

// redTeamCall 经门面发一次原生调用：帧头 work / from 由调用方指定，错误原样返回。
func redTeamCall(t *testing.T, url, work, from, method string, input any) ([]byte, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, err := spv2.Dial(ctx, url)
	if err != nil {
		t.Fatalf("连接 %s 失败: %v", url, err)
	}
	defer func() { _ = conn.Close() }()
	call, err := spv2.NewCall(work, spv2.ServiceRef(from), spv2.ServiceRef("memory"), method, input)
	if err != nil {
		t.Fatalf("构造 %s 调用失败: %v", method, err)
	}
	resp, err := conn.Call(ctx, call)
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func redTeamStore(t *testing.T, url, work, from, content, key string) (protocol.StoreResult, error) {
	t.Helper()
	body, err := redTeamCall(t, url, work, from, "store", protocol.StorePayload{
		ConversationID: "conv-" + work, Work: work, Role: "user",
		Content: content, ClientItemID: key,
	})
	if err != nil {
		return protocol.StoreResult{}, err
	}
	var res protocol.StoreResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatalf("store 回执无法解码: %v", err)
	}
	return res, nil
}

func redTeamStoreMessage(t *testing.T, url, work, from, content string) string {
	t.Helper()
	res, err := redTeamStore(t, url, work, from, content, "ci-red-"+spv2.NewID())
	if err != nil {
		t.Fatalf("store（%s）失败: %v", work, err)
	}
	if res.ItemID == "" {
		t.Fatal("store 应返回 item_id（context_v2 的 trigger.item 靠它）")
	}
	return res.ItemID
}

func redTeamStatus(t *testing.T, url, work, from, callID string) (protocol.CallStatusResult, error) {
	t.Helper()
	body, err := redTeamCall(t, url, work, from, "call_status", protocol.CallStatusPayload{
		IDs: []string{callID},
	})
	if err != nil {
		return protocol.CallStatusResult{}, err
	}
	var res protocol.CallStatusResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatalf("call_status 回执无法解码: %v", err)
	}
	return res, nil
}

func redTeamResidentCount(t *testing.T, url, work, from string) int {
	t.Helper()
	body, err := redTeamCall(t, url, work, from, "resident_list", map[string]string{"work": work})
	if err != nil {
		t.Fatalf("resident_list（%s）失败: %v", work, err)
	}
	var res protocol.ResidentListResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatalf("resident_list 回执无法解码: %v", err)
	}
	return len(res.Items)
}

func redTeamHistoryContains(t *testing.T, url, conversation, want string) bool {
	t.Helper()
	body, err := redTeamCall(t, url, "w-red-alice", "reasoner-a", "history", protocol.HistoryPayload{
		ConversationID: conversation,
	})
	if err != nil {
		t.Fatalf("history（%s）失败: %v", conversation, err)
	}
	var res protocol.HistoryResult
	if err := protocol.DecodeRaw(body, &res); err != nil {
		t.Fatalf("history 回执无法解码: %v", err)
	}
	for _, m := range res.Messages {
		if strings.Contains(m.Content, want) {
			return true
		}
	}
	return false
}

// redTeamAnonymousStore 用裸 WS 手写一个 from.id 为空的 store call：
// spv2.NewCall / Client.Call 都会在客户端校验 from，测试只能绕过客户端构造器发帧。
// 返回服务端的错误 reply（帧被拒也是显式失败，不是静默丢弃）。
func redTeamAnonymousStore(t *testing.T, url, work, content, key string) error {
	t.Helper()
	raw, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("裸 WS 连接 %s 失败: %v", url, err)
	}
	defer func() { _ = raw.Close() }()
	_ = raw.SetReadDeadline(time.Now().Add(10 * time.Second))

	// 连接首帧必须是 hello。
	hello, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef("e2e-anon"), spv2.ServiceRef("memory"), "hello",
		map[string]any{"protocol": spv2.ProtocolVersion})
	if err != nil {
		t.Fatal(err)
	}
	helloRaw, err := spv2.Marshal(hello)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.WriteMessage(websocket.BinaryMessage, helloRaw); err != nil {
		t.Fatal(err)
	}
	if _, _, err := raw.ReadMessage(); err != nil {
		t.Fatalf("hello 回执读取失败: %v", err)
	}

	payload, err := spv2.EncodeBody(protocol.StorePayload{
		ConversationID: "conv-" + work, Work: work, Role: "user",
		Content: content, ClientItemID: key,
	})
	if err != nil {
		t.Fatal(err)
	}
	anonymous := spv2.Frame{
		V: spv2.ProtocolVersion, ID: spv2.NewID(), Type: spv2.TypeCall, Work: work,
		From: spv2.Ref{Kind: "service"}, To: spv2.ServiceRef("memory"),
		Method: "store", ContentType: spv2.ContentTypeMsgPack, Body: payload,
	}
	encoded, err := spv2.Marshal(anonymous)
	if err != nil {
		t.Fatal(err)
	}
	if err := raw.WriteMessage(websocket.BinaryMessage, encoded); err != nil {
		t.Fatal(err)
	}
	_, data, err := raw.ReadMessage()
	if err != nil {
		t.Fatalf("匿名 store 应显式回错误 reply（不是静默断开）: %v", err)
	}
	f, err := spv2.Unmarshal(data)
	if err != nil {
		t.Fatalf("匿名 store 回执无法解码: %v", err)
	}
	if f.Status != spv2.StatusError || f.Error == nil {
		t.Fatalf("匿名 store 应回错误 reply，实际 status=%q error=%+v", f.Status, f.Error)
	}
	return &spv2.Error{Code: f.Error.Code, Message: f.Error.Message, Retryable: f.Error.Retryable, Details: f.Error.Details}
}

// expectRedTeamCode 断言业务错误码。
func expectRedTeamCode(t *testing.T, err error, code, label string) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s 必须被拒绝，实际成功", label)
	}
	if !spv2.IsCode(err, code) {
		t.Fatalf("%s 应 %s，实际: %v", label, code, err)
	}
}
