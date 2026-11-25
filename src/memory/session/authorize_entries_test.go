package session

// v2 入口的授权接线测试（R-023 P4）：证明四个入口与 call_control / call_status /
// resident_list 都是「先授权再读写」，且拒绝路径零写入。
//
// 这里不启动 WS：handler 只把 *spv2.Call 的帧头收成 caller 后转给共享实现，
// 真正的线上收发由 src/memory/tests 与 testing/tests 的三进程用例覆盖。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// newAuthorizeEntriesServer 建两条 work（alice / bob）与 bob 的任务，
// 供各入口构造「帧头自己的 work、载荷别人的 work」的越权场景。
func newAuthorizeEntriesServer(t *testing.T) (*Server, *store.Store, string) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	for _, w := range []spv2.Work{
		{
			ID: "w-alice", Root: "w-alice", Class: spv2.ClassRequest,
			Owner: spv2.HumanRef("alice"), Agent: spv2.AgentRef("reasoner-a"),
			Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
		},
		{
			ID: "w-bob", Root: "w-bob", Class: spv2.ClassRequest,
			Owner: spv2.HumanRef("bob"), Agent: spv2.AgentRef("reasoner-b"),
			Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
		},
	} {
		if _, err := st.EnsureWork(ctx, w); err != nil {
			t.Fatalf("登记 work %s: %v", w.ID, err)
		}
	}
	task, err := st.OpenTask("conv-bob", "bob 的任务")
	if err != nil {
		t.Fatalf("建 bob 任务: %v", err)
	}
	if err := st.LinkWorkTask(ctx, "w-bob", task.ID); err != nil {
		t.Fatalf("给 bob 的 work 挂任务失败: %v", err)
	}
	return &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}, st, task.ID
}

func entryPayload(t *testing.T, v any) []byte {
	t.Helper()
	body, err := protocol.EncodePayload(v)
	if err != nil {
		t.Fatalf("编码载荷: %v", err)
	}
	return body
}

func entryCount(t *testing.T, st *store.Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("查询 %q: %v", query, err)
	}
	return n
}

var (
	callerAlice     = caller{work: "w-alice", from: spv2.AgentRef("reasoner-a")}
	callerBob       = caller{work: "w-bob", from: spv2.AgentRef("reasoner-b")}
	callerAnonymous = caller{work: "w-bob", from: spv2.Ref{Kind: "service"}}
)

// TestEntryAuthorizationStoreV2 v2 store：跨 work / 匿名一律 permission_denied，且零写入；
// 本 work 的 agent 可以正常写（证明授权没有把正常路径一起挡掉）。
func TestEntryAuthorizationStoreV2(t *testing.T) {
	s, st, _ := newAuthorizeEntriesServer(t)
	body := entryPayload(t, protocol.StorePayload{
		ConversationID: "conv-bob", Work: "w-bob", Role: "user",
		Content: "冒充 bob 写入", ClientItemID: "ci-cross",
	})

	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.store(body, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
			if n := entryCount(t, st, `SELECT COUNT(*) FROM messages WHERE work_id = ?`, "w-bob"); n != 0 {
				t.Fatalf("越权 store 不应落消息，实际 %d 行", n)
			}
			if n := entryCount(t, st, `SELECT COUNT(*) FROM store_receipts WHERE work = ?`, "w-bob"); n != 0 {
				t.Fatalf("越权 store 不应留幂等回执，实际 %d 行", n)
			}
		})
	}

	// 本 work 的 agent 正常写：授权不能把正常链路一起挡掉。
	out, err := s.store(body, callerBob)
	if err != nil {
		t.Fatalf("本 work 的 agent 应能写: %v", err)
	}
	if res, ok := out.(protocol.StoreResult); !ok || res.ItemID == "" {
		t.Fatalf("正常写入应回 StoreResult，实际 %T %+v", out, out)
	}
	if n := entryCount(t, st, `SELECT COUNT(*) FROM messages WHERE work_id = ?`, "w-bob"); n != 1 {
		t.Fatalf("正常写入应落 1 条消息，实际 %d 行", n)
	}

	// 目标 work 不存在：not_found（不是 permission_denied，避免把「不存在」也说成越权）。
	ghost := entryPayload(t, protocol.StorePayload{
		ConversationID: "conv-ghost", Work: "w-ghost", Role: "user",
		Content: "写幽灵 work", ClientItemID: "ci-ghost",
	})
	if _, err := s.store(ghost, caller{work: "w-ghost", from: spv2.AgentRef("reasoner-a")}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("work 不存在应 not_found，实际: %v", err)
	}
}

// TestEntryAuthorizationContextAndTrigger：context_base / trigger_complete 在读触发、落快照、
// 改状态之前授权；越权读取不产生快照，越权完成不改 trigger_states。
func TestEntryAuthorizationContextAndTrigger(t *testing.T) {
	s, st, taskID := newAuthorizeEntriesServer(t)
	ctx := context.Background()
	item := insertContextBaseMessage(t, st, "w-bob", taskID, "user", "user", "bob 的触发", "", 1)
	reqBody := entryPayload(t, protocol.ContextRequest{
		Work: "w-bob", Trigger: protocol.Trigger{Kind: "user", Item: item}, Schema: contextBaseSchema,
	})

	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run("context_base "+tc.name, func(t *testing.T) {
			if _, err := s.contextBase(ctx, reqBody, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
			if n := entryCount(t, st, `SELECT COUNT(*) FROM context_snapshots WHERE work = ?`, "w-bob"); n != 0 {
				t.Fatalf("越权读取不应产出快照，实际 %d 份", n)
			}
			if state, _, err := st.GetTriggerState(ctx, "w-bob", "user", item); err != nil || state != "" {
				t.Fatalf("越权读取不应写触发状态: state=%q err=%v", state, err)
			}
		})
	}

	resp, err := s.contextBase(ctx, reqBody, callerBob)
	if err != nil {
		t.Fatalf("bob 自己读应成功: %v", err)
	}
	assembled, ok := resp.(protocol.ContextResponse)
	if !ok || assembled.SnapshotID == "" {
		t.Fatalf("应返回带 snapshot_id 的上下文，实际 %T %+v", resp, resp)
	}

	completeBody := entryPayload(t, protocol.TriggerCompletePayload{
		Work: "w-bob", Trigger: protocol.Trigger{Kind: "user", Item: item},
	})
	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run("trigger_complete "+tc.name, func(t *testing.T) {
			if _, err := s.triggerComplete(ctx, completeBody, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
			if state, _, err := st.GetTriggerState(ctx, "w-bob", "user", item); err != nil || state != store.TriggerStateAssembled {
				t.Fatalf("越权完成不得改写触发状态: state=%q err=%v", state, err)
			}
		})
	}
	if _, err := s.triggerComplete(ctx, completeBody, callerBob); err != nil {
		t.Fatalf("bob 自己完成应成功: %v", err)
	}

	// 目标 work 不存在：not_found，且错误发生在任何触发读写之前。
	ghostCtx := entryPayload(t, protocol.ContextRequest{
		Work: "w-ghost", Trigger: protocol.Trigger{Kind: "user", Item: item},
	})
	if _, err := s.contextBase(ctx, ghostCtx, caller{work: "w-ghost", from: spv2.AgentRef("reasoner-a")}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("context_base 的 work 不存在应 not_found，实际: %v", err)
	}
	ghostTrigger := entryPayload(t, protocol.TriggerCompletePayload{
		Work: "w-ghost", Trigger: protocol.Trigger{Kind: "user", Item: item},
	})
	if _, err := s.triggerComplete(ctx, ghostTrigger, caller{work: "w-ghost", from: spv2.AgentRef("reasoner-a")}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("trigger_complete 的 work 不存在应 not_found，实际: %v", err)
	}
}

// TestEntryAuthorizationCallOpenAndControl：call_open 先授权再落 call 行；
// call_control 在 control_id 重放之前按目标 call 的 work 授权，越权不改状态、不留控制记录。
func TestEntryAuthorizationCallOpenAndControl(t *testing.T) {
	s, st, _ := newAuthorizeEntriesServer(t)
	ctx := context.Background()
	openBody := entryPayload(t, protocol.CallOpenPayload{
		Work: "w-bob", Service: "shell", Action: "run",
		Mode: store.CallModeResident, Pausable: true, TTLMS: 60000, ExecDeadlineMS: 300000,
	})

	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run("call_open "+tc.name, func(t *testing.T) {
			if _, err := s.openCall(ctx, openBody, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
			if n := entryCount(t, st, `SELECT COUNT(*) FROM calls WHERE work = ?`, "w-bob"); n != 0 {
				t.Fatalf("越权 call_open 不应落调用，实际 %d 行", n)
			}
		})
	}

	out, err := s.openCall(ctx, openBody, callerBob)
	if err != nil {
		t.Fatalf("bob 自己 open 应成功: %v", err)
	}
	callID := out.CallID
	if callID == "" {
		t.Fatal("call_open 应返回 call_id")
	}

	controlBody := entryPayload(t, protocol.CallControlPayload{
		ID: callID, ControlID: "ctl-cross", Op: "cancel",
	})
	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run("call_control "+tc.name, func(t *testing.T) {
			if _, err := s.controlCall(ctx, controlBody, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
			row, err := st.GetCall(ctx, callID)
			if err != nil {
				t.Fatal(err)
			}
			if row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecCreated {
				t.Fatalf("越权控制不得改目标 call: %+v", row)
			}
			if n := entryCount(t, st, `SELECT COUNT(*) FROM call_controls WHERE call_id = ?`, callID); n != 0 {
				t.Fatalf("越权控制不应留控制记录，实际 %d 行", n)
			}
		})
	}
	if _, err := s.controlCall(ctx, controlBody, callerBob); err != nil {
		t.Fatalf("bob 自己取消应成功: %v", err)
	}

	// 目标 work 不存在：not_found（授权查 work 时先于落库）。
	ghostOpen := entryPayload(t, protocol.CallOpenPayload{Work: "w-ghost", Service: "shell"})
	if _, err := s.openCall(ctx, ghostOpen, caller{work: "w-ghost", from: spv2.AgentRef("reasoner-a")}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("call_open 的 work 不存在应 not_found，实际: %v", err)
	}
}

// TestEntryAuthorizationStatusAndResident：call_status 逐 id 校验归属，任一越权整体拒绝；
// resident_list 按 work 的 agent/owner/entry 授权，匿名拒绝。
func TestEntryAuthorizationStatusAndResident(t *testing.T) {
	s, _, _ := newAuthorizeEntriesServer(t)
	ctx := context.Background()
	out, err := s.openCall(ctx, entryPayload(t, protocol.CallOpenPayload{
		Work: "w-bob", Service: "shell", Action: "run",
		Mode: store.CallModeResident, Pausable: true,
	}), callerBob)
	if err != nil {
		t.Fatalf("bob 自己 open 应成功: %v", err)
	}
	callID := out.CallID

	statusBody := entryPayload(t, protocol.CallStatusPayload{IDs: []string{callID}})
	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run("call_status "+tc.name, func(t *testing.T) {
			if _, err := s.callStatus(ctx, statusBody, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
		})
	}
	res, err := s.callStatus(ctx, statusBody, callerBob)
	if err != nil {
		t.Fatalf("bob 自己读状态应成功: %v", err)
	}
	if len(res.Items) != 1 || res.Items[0].ID != callID {
		t.Fatalf("本 work 的对账项应原样返回: %+v", res.Items)
	}

	residentReq := residentListRequest{Work: "w-bob"}
	for _, tc := range []struct {
		name string
		c    caller
	}{
		{"跨 work", callerAlice},
		{"匿名", callerAnonymous},
	} {
		t.Run("resident_list "+tc.name, func(t *testing.T) {
			if _, err := s.residentListFor(ctx, residentReq, tc.c); !spv2.IsCode(err, spv2.CodePermissionDenied) {
				t.Fatalf("应 permission_denied，实际: %v", err)
			}
		})
	}
	residents, err := s.residentListFor(ctx, residentReq, callerBob)
	if err != nil {
		t.Fatalf("bob 自己列常驻应成功: %v", err)
	}
	if len(residents.Items) != 1 || residents.Items[0].ID != callID {
		t.Fatalf("本 work 的常驻项应返回: %+v", residents.Items)
	}

	// 目标 work 不存在：not_found（resident_list 与 call_status 的未知 id 边界分开）。
	if _, err := s.residentListFor(ctx, residentListRequest{Work: "w-ghost"}, callerBob); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("resident_list 的 work 不存在应 not_found，实际: %v", err)
	}
	if _, err := s.callStatus(ctx, entryPayload(t, protocol.CallStatusPayload{IDs: []string{"ghost-call"}}), callerBob); err != nil {
		t.Fatalf("call_status 的未知 id 只回 id，不应失败: %v", err)
	}
}
