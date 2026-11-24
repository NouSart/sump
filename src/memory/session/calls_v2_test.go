package session

// calls_v2 的同包单测：method 声明（非工具 / schema）、call_open 的初始两维与参数校验、
// call_control 的诚实失败（pause/resume 未接 owner → unavailable，状态不变）与取消幂等、
// call_status 的本地账本读取、resident_list 的 owner 隔离与分页。
//
// 与源 af6cfc8:src/memory/server/calls_v2_test.go 的差异（R-023 P3）：
//   - image_note 两个用例不搬（D7：image_index 不在本架构）；
//   - 跨 work 越权用例不搬（授权在 P4），本阶段按载荷 work 操作；
//   - 保留 7 个等价用例，并补 2 条本阶段的真实错误路径（未知 call / 非法 op /
//     空参数；同 control_id 换 call 的冲突且不泄漏 id），共 9 个；
//   - origin_session 断言改为「必须为空」：不透明会话 ID 在 experience/redact，
//     session 不得 import experience（架构测试），本笔不复制第二套脱敏实现；
//   - work 关联 task 不再手工挂（works.task 的写入方是会话生命周期），call 行的
//     owner/task 联表字段由 store 层用例覆盖。
//
// handler 的线上收发（*spv2.Call）需要真启动服务，属于 memory/tests 的端到端范围；
// 这里直接测 handler 背后的逻辑层，保证语义与存储落点可证。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

func newCallsTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}, st
}

// seedCallsWork 登记一条 work（owner 用来构造常驻目录隔离）。
func seedCallsWork(t *testing.T, st *store.Store, id, owner, agent string) {
	t.Helper()
	if _, err := st.EnsureWork(context.Background(), spv2.Work{
		ID: id, Root: id, Class: spv2.ClassRequest,
		Owner: spv2.HumanRef(owner), Agent: spv2.AgentRef(agent),
		Entry: spv2.ServiceRef("frontend"), Status: spv2.WorkRunning,
	}); err != nil {
		t.Fatalf("登记 work %s: %v", id, err)
	}
}

// callOpenBody / callControlBody / callStatusBody 是逻辑层的入参（body 即线路契约）。
func callOpenBody(t *testing.T, req protocol.CallOpenPayload) []byte {
	t.Helper()
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func callControlBody(t *testing.T, req protocol.CallControlPayload) []byte {
	t.Helper()
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func callStatusBody(t *testing.T, req protocol.CallStatusPayload) []byte {
	t.Helper()
	body, err := protocol.EncodePayload(req)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func controlCalls(t *testing.T, s *Server, req protocol.CallControlPayload) (protocol.CallControlResult, error) {
	t.Helper()
	return s.controlCall(context.Background(), callControlBody(t, req))
}

func statusCalls(t *testing.T, s *Server, req protocol.CallStatusPayload) (protocol.CallStatusResult, error) {
	t.Helper()
	return s.callStatus(context.Background(), callStatusBody(t, req))
}

func openCallsTestCall(t *testing.T, s *Server, work, mode string, pausable bool) string {
	t.Helper()
	res, err := s.openCall(context.Background(), callOpenBody(t, protocol.CallOpenPayload{
		Work: work, Service: "shell", Action: "run", Mode: mode,
		Pausable: pausable, TTLMS: 60000, ExecDeadlineMS: 300000,
	}))
	if err != nil {
		t.Fatalf("openCall(%s): %v", work, err)
	}
	return res.CallID
}

// TestCallsV2MethodDeclarations 注册声明：方法名、非工具、schema 是 object 且必填项齐全。
func TestCallsV2MethodDeclarations(t *testing.T) {
	methods := []spv2.Method{
		callOpenMethod(), callControlMethod(), callStatusMethod(), residentListMethod(),
	}
	wantNames := map[string]bool{
		"call_open": false, "call_control": false, "call_status": false, "resident_list": false,
	}
	for _, m := range methods {
		tool, ok := wantNames[m.Name]
		if !ok {
			t.Fatalf("未预期的 method %q", m.Name)
		}
		delete(wantNames, m.Name)
		if m.Tool != tool {
			t.Errorf("%s 不应暴露给模型（Tool 必须为 false）", m.Name)
		}
		if m.Input == nil || m.Input["type"] != "object" || m.Output["type"] != "object" {
			t.Errorf("%s 的 input/output 必须是 object schema: %+v / %+v", m.Name, m.Input, m.Output)
		}
	}
	if len(wantNames) != 0 {
		t.Fatalf("缺少 method 声明: %v", wantNames)
	}
	// 必填项：call_open 要 work + service；call_control 要 id / control_id / op；
	// call_status 要 ids；resident_list 的 work 可省略（回退帧头 work）。
	for _, field := range []string{"work", "service"} {
		if !schemaRequires(callOpenMethod().Input, field) {
			t.Fatalf("call_open 的 input 应必填 %s: %+v", field, callOpenMethod().Input)
		}
	}
	for _, field := range []string{"id", "control_id", "op"} {
		if !schemaRequires(callControlMethod().Input, field) {
			t.Fatalf("call_control 的 input 应必填 %s: %+v", field, callControlMethod().Input)
		}
	}
	for _, field := range []string{"call_id"} {
		if !schemaRequires(callOpenMethod().Output, field) {
			t.Fatalf("call_open 的 output 应必填 %s: %+v", field, callOpenMethod().Output)
		}
	}
	if !schemaRequires(callStatusMethod().Output, "owner_recovery_complete") {
		t.Fatalf("call_status 的 output 应必填 owner_recovery_complete: %+v", callStatusMethod().Output)
	}

	// 注册本身可调用（重复注册是覆盖，不 panic）。这里用一个未启动的 service 骨架。
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = st.Close() }()
	s := &Server{
		Service: service.New(service.Config{Name: "memory-test", Logger: log.New(io.Discard, "", 0)}),
		cfg:     Config{Store: st},
		logger:  log.New(io.Discard, "", 0),
	}
	s.registerCallsV2()
	s.registerCallsV2()
}

func TestOpenCallInitialState(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	id := openCallsTestCall(t, s, "w1", store.CallModeResident, true)
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecCreated || row.Phase != "" {
		t.Fatalf("call_open 初始两维不对: %+v", row)
	}
	if row.Owner != "alice" || row.Service != "shell" || row.Mode != store.CallModeResident || !row.Pausable {
		t.Fatalf("call 行字段不对: %+v", row)
	}

	// 省略 mode 按 once 落库。
	id2 := openCallsTestCall(t, s, "w1", "", false)
	if row, err := st.GetCall(ctx, id2); err != nil || row.Mode != store.CallModeOnce {
		t.Fatalf("缺省 mode 应为 once: %+v, err=%v", row, err)
	}
}

func TestOpenCallArgValidation(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	cases := []struct {
		name string
		req  protocol.CallOpenPayload
		code string
	}{
		{"work 为空", protocol.CallOpenPayload{Service: "shell"}, spv2.CodeInvalidArgument},
		{"service 为空", protocol.CallOpenPayload{Work: "w1"}, spv2.CodeInvalidArgument},
		{"负 ttl", protocol.CallOpenPayload{Work: "w1", Service: "shell", TTLMS: -1}, spv2.CodeInvalidArgument},
		{"负 exec_deadline", protocol.CallOpenPayload{Work: "w1", Service: "shell", ExecDeadlineMS: -1}, spv2.CodeInvalidArgument},
		{"非法 mode", protocol.CallOpenPayload{Work: "w1", Service: "shell", Mode: "forever"}, spv2.CodeInvalidArgument},
		{"work 不存在", protocol.CallOpenPayload{Work: "nope", Service: "shell"}, spv2.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.openCall(ctx, callOpenBody(t, c.req))
			if !spv2.IsCode(err, c.code) {
				t.Fatalf("期望 %s，实际 %v", c.code, err)
			}
		})
	}

	// 校验失败不得留下任何 call 行（参数错误不是"先落库再说"）。
	var n int
	if err := st.DB().QueryRow(`SELECT COUNT(*) FROM calls`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("参数校验失败不应落 call，实际 %d 条", n)
	}
}

func TestCallControlPauseUnavailableKeepsState(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeResident, true)

	_, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-pause", Op: "pause"})
	if !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("pause 未接 owner 应 unavailable: %v", err)
	}
	if row, err := st.GetCall(ctx, id); err != nil || row.Observe != store.CallObserveWaiting ||
		row.Exec != store.CallExecCreated || row.Phase != "" {
		t.Fatalf("pause 失败不得改状态: %+v, err=%v", row, err)
	}

	// 失败不留下 control 记录：同 control_id 重试仍是 unavailable，而不是"已完成"。
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-pause", Op: "pause"}); !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("pause 重试应仍 unavailable: %v", err)
	}
	if _, ok, err := st.GetControl(ctx, "ctl-pause"); ok || err != nil {
		t.Fatalf("失败的 pause 不应落 control: ok=%v err=%v", ok, err)
	}

	// resume 只能从 paused 出发；created 直接 resume 是 conflict。
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-resume-1", Op: "resume"}); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("created 上 resume 应 conflict: %v", err)
	}

	// 构造 paused（直接落库，模拟 owner 已确认的既有状态），resume 仍需 owner 握手。
	if err := st.SetCallState(ctx, id, store.CallObserveWaiting, store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallState(ctx, id, store.CallObserveWaiting, store.CallExecPaused, "", "pause_confirmed"); err != nil {
		t.Fatal(err)
	}
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-resume-2", Op: "resume"}); !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("resume 未接 owner 应 unavailable: %v", err)
	}
	if row, _ := st.GetCall(ctx, id); row.Exec != store.CallExecPaused {
		t.Fatalf("resume 失败不得改状态: %+v", row)
	}
}

func TestCallControlCancelAndIdempotency(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	// 可暂停调用：本地取消 = 执行侧已取消。
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	first, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-cancel", Op: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if first.Observe != store.CallObserveClosed || first.Exec != store.CallExecCancelled || first.Reason != "cancelled" {
		t.Fatalf("cancel 结果不对: %+v", first)
	}
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Phase != store.CallPhaseCancelled || row.RemoteMayContinue {
		t.Fatalf("可暂停取消的落库不对: %+v", row)
	}

	// 同 control_id 重放：结果一致，且不会因第一次已关闭而变成 conflict。
	replay, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-cancel", Op: "cancel"})
	if err != nil {
		t.Fatalf("同 control_id 重放应成功: %v", err)
	}
	if replay.Observe != first.Observe || replay.Exec != first.Exec || replay.Reason != first.Reason {
		t.Fatalf("重放结果不一致: %+v vs %+v", replay, first)
	}
	// 换个 control_id 再取消已关闭的调用：conflict（不能假装还有效）。
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id, ControlID: "ctl-cancel-2", Op: "cancel"}); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("已关闭调用再取消应 conflict: %v", err)
	}

	// 不可暂停调用：取消后 exec=unknown、不写 phase、remote_may_continue=true。
	id2 := openCallsTestCall(t, s, "w1", store.CallModeOnce, false)
	res2, err := controlCalls(t, s, protocol.CallControlPayload{ID: id2, ControlID: "ctl-cancel-np", Op: "cancel"})
	if err != nil {
		t.Fatal(err)
	}
	if res2.Observe != store.CallObserveClosed || res2.Exec != store.CallExecUnknown || res2.Reason != "remote_may_continue" {
		t.Fatalf("不可暂停取消结果不对: %+v", res2)
	}
	row2, err := st.GetCall(ctx, id2)
	if err != nil {
		t.Fatal(err)
	}
	if !row2.RemoteMayContinue || row2.Phase != "" {
		t.Fatalf("不可暂停取消的落库不对: %+v", row2)
	}
}

// TestCallControlErrorPaths 钉住参数与归属的错误码：未知 call / 非法 op / 空参数。
func TestCallControlErrorPaths(t *testing.T) {
	s, st := newCallsTestServer(t)
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	cases := []struct {
		name string
		req  protocol.CallControlPayload
		code string
	}{
		{"空 id", protocol.CallControlPayload{ControlID: "c", Op: "cancel"}, spv2.CodeInvalidArgument},
		{"空 control_id", protocol.CallControlPayload{ID: "x", Op: "cancel"}, spv2.CodeInvalidArgument},
		{"空 op", protocol.CallControlPayload{ID: "x", ControlID: "c"}, spv2.CodeInvalidArgument},
		{"非法 op", protocol.CallControlPayload{ID: "x", ControlID: "c", Op: "restart"}, spv2.CodeInvalidArgument},
		{"未知 call", protocol.CallControlPayload{ID: "nope", ControlID: "c", Op: "cancel"}, spv2.CodeNotFound},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := controlCalls(t, s, c.req); !spv2.IsCode(err, c.code) {
				t.Fatalf("期望 %s，实际 %v", c.code, err)
			}
		})
	}
}

// TestCallControlIDConflictDoesNotLeak 同 control_id 换 call / op 一律 conflict，且错误文案不回显目标 id。
func TestCallControlIDConflictDoesNotLeak(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	id1 := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	if _, err := controlCalls(t, s, protocol.CallControlPayload{ID: id1, ControlID: "ctl-shared", Op: "cancel"}); err != nil {
		t.Fatal(err)
	}
	id2 := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)
	_, err := controlCalls(t, s, protocol.CallControlPayload{ID: id2, ControlID: "ctl-shared", Op: "cancel"})
	if !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("同 control_id 换 call 应 conflict: %v", err)
	}
	if got := err.Error(); strings.Contains(got, id1) || strings.Contains(got, id2) {
		t.Fatalf("冲突文案不得回显 call id: %q", got)
	}
	// 目标 call 未被这次冲突改动。
	if row, _ := st.GetCall(ctx, id2); row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecCreated {
		t.Fatalf("冲突不应改动目标 call: %+v", row)
	}
}

func TestCallStatusReportsLocalLedgerOnly(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, "w1", store.CallModeOnce, true)

	res, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: []string{id, "missing-call", " "}})
	if err != nil {
		t.Fatal(err)
	}
	if res.OwnerRecoveryComplete {
		t.Fatal("owner 对账未接入，owner_recovery_complete 必须显式 false")
	}
	if len(res.Items) != 2 { // 空白 id 被跳过
		t.Fatalf("items = %+v", res.Items)
	}
	if res.Items[0].ID != id || res.Items[0].Observe != store.CallObserveWaiting ||
		res.Items[0].Exec != store.CallExecCreated || res.Items[0].OwnerService != "shell" {
		t.Fatalf("已知 call 的对账项不对: %+v", res.Items[0])
	}
	// 查不到的 id 只回 id；不得判成 orphaned（问不到 ≠ 不存在）。
	if res.Items[1].ID != "missing-call" || res.Items[1].Exec != "" || res.Items[1].Phase != "" {
		t.Fatalf("未知 call 的对账项不应被判定: %+v", res.Items[1])
	}

	// 只读：对账不回写本地账本。
	if row, _ := st.GetCall(ctx, id); row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecCreated {
		t.Fatalf("对账不应改本地账本: %+v", row)
	}

	// 空 ids / 超过 100 个：参数错误。
	if _, err := statusCalls(t, s, protocol.CallStatusPayload{}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("空 ids 应 invalid_argument: %v", err)
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = "x"
	}
	if _, err := statusCalls(t, s, protocol.CallStatusPayload{IDs: many}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("超过 100 个 id 应 invalid_argument: %v", err)
	}
}

func TestResidentListFiltersByOwner(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w-alice", "alice", "reasoner")
	seedCallsWork(t, st, "w-bob", "bob", "reasoner-b")

	a1 := openCallsTestCall(t, s, "w-alice", store.CallModeResident, true)
	time.Sleep(time.Millisecond) // 让 created_at 明确分先后
	a2 := openCallsTestCall(t, s, "w-alice", store.CallModeResident, true)
	_ = openCallsTestCall(t, s, "w-alice", store.CallModeOnce, true) // 非常驻不出现
	_ = openCallsTestCall(t, s, "w-bob", store.CallModeResident, true)

	res, err := s.residentListFor(ctx, residentListRequest{Work: "w-alice", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 2 {
		t.Fatalf("alice 常驻调用 = %d, want 2（once 与 bob 的 resident 都不应出现）: %+v", len(res.Items), res.Items)
	}
	if res.Items[0].ID != a2 || res.Items[1].ID != a1 {
		t.Fatalf("应按 created_at 倒序: %+v", res.Items)
	}
	for _, item := range res.Items {
		if item.Kind != "call" || item.OwnerService != "shell" || item.Exec == "" {
			t.Fatalf("常驻项字段不对: %+v", item)
		}
		// 不透明会话 ID 留给 experience/redact：这里必须为空，不能是原文。
		if item.OriginSession != "" {
			t.Fatalf("origin_session 本笔应为空（脱敏实现不在 session）: %q", item.OriginSession)
		}
	}

	// 分页：limit=1 时 next 指向下一页游标。
	page1, err := s.residentListFor(ctx, residentListRequest{Work: "w-alice", Limit: 1})
	if err != nil || len(page1.Items) != 1 || page1.Items[0].ID != a2 || page1.Next != a2 {
		t.Fatalf("第一页 = %+v, err=%v", page1, err)
	}
	page2, err := s.residentListFor(ctx, residentListRequest{Work: "w-alice", Limit: 1, Cursor: page1.Next})
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ID != a1 {
		t.Fatalf("第二页 = %+v, err=%v", page2, err)
	}

	// 解析不到调用方 work：invalid_argument（不是空列表）。
	if _, err := s.residentListFor(ctx, residentListRequest{}); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("缺 work 应 invalid_argument: %v", err)
	}
	if _, err := s.residentListFor(ctx, residentListRequest{Work: "nope"}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("work 不存在应 not_found: %v", err)
	}
}
