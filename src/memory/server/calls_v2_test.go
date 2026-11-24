package server

// calls_v2 的同包单测：注册清单（Tool / schema）、call_open 校验、call_control
// 的幂等与诚实失败（pause/resume 未接 owner → unavailable，状态不变）、
// resident_list 的 owner 隔离、image_note 的脱敏 / 截断 / 越权拒绝。
//
// handler 的线上收发（*spv2.Call）需要真启动服务，属于 memory/tests 的端到端范围；
// 这里直接测 handler 背后的逻辑层，保证语义与存储落点可证。

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func newCallsTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{Store: st}, log.New(io.Discard, "", 0))
	// server.New 的接线由主代理完成；测试里显式调用，重复注册是 map 覆盖（幂等）。
	s.registerCallsV2()
	return s, st
}

// seedCallsWork 登记一条 work；owner / agent 用来构造 owner 隔离与越权用例。
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

// linkCallsTestTask 给 work 关联一个任务（image_note 靠 works.task → conversation 解析会话）。
func linkCallsTestTask(t *testing.T, st *store.Store, work, conversation string) protocol.Task {
	t.Helper()
	task, err := st.OpenTask(conversation, "调用测试任务")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.LinkWorkTask(context.Background(), work, task.ID); err != nil {
		t.Fatal(err)
	}
	return task
}

// workAgentRef 取已登记 work 的 agent 引用，让测试用「这条 work 自己的 agent」身份调用。
func workAgentRef(t *testing.T, st *store.Store, work string) spv2.Ref {
	t.Helper()
	w, err := st.GetWork(context.Background(), work)
	if err != nil {
		t.Fatalf("读取 work %s: %v", work, err)
	}
	return w.Agent
}

func openCallsTestCall(t *testing.T, s *Server, st *store.Store, work, mode string, pausable bool) string {
	t.Helper()
	res, err := s.openCall(context.Background(), protocol.CallOpenPayload{
		Work: work, Service: "shell", Action: "run", Mode: mode,
		Pausable: pausable, TTLMS: 60000, ExecDeadlineMS: 300000,
	}, work, workAgentRef(t, st, work))
	if err != nil {
		t.Fatalf("openCall(%s): %v", work, err)
	}
	return res.CallID
}

func TestCallsV2MethodRegistry(t *testing.T) {
	want := map[string]bool{
		"call_open":     false,
		"call_control":  false,
		"call_status":   false,
		"resident_list": false,
		"image_note":    true, // S-001 §3.3：只有 image_note 是工具
	}
	s, _ := newCallsTestServer(t)
	got := map[string]spv2.Method{}
	for _, m := range s.NativeMethods() {
		got[m.Name] = m
	}
	for name, tool := range want {
		m, ok := got[name]
		if !ok {
			t.Fatalf("未注册的 method %q", name)
		}
		if m.Tool != tool {
			t.Fatalf("method %s 的 Tool=%v, want %v", name, m.Tool, tool)
		}
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Fatalf("method %s 缺 Input/Output schema", name)
		}
	}
}

func TestOpenCallValidatesWorkAndTask(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	task := linkCallsTestTask(t, st, "w1", "conv-1")

	id := openCallsTestCall(t, s, st, "w1", store.CallModeResident, true)
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecCreated || row.Phase != "" {
		t.Fatalf("call_open 初始两维不对: %+v", row)
	}
	if row.Owner != "alice" || row.Task != task.ID {
		t.Fatalf("owner/task 解析不对: owner=%q task=%q", row.Owner, row.Task)
	}

	// work 不存在 → not_found。
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "nope", Service: "shell"}, "nope", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("缺 work 应 not_found: %v", err)
	}
	// service 必填、ttl 不能为负、mode 只允许 once/resident。
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w1"}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("缺 service 应 invalid_argument: %v", err)
	}
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w1", Service: "shell", TTLMS: -1}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("负 ttl 应 invalid_argument: %v", err)
	}
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w1", Service: "shell", Mode: "forever"}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非法 mode 应 invalid_argument: %v", err)
	}
}

// TestOpenCallRejectsCrossWork 固化 v2 入口的跨 work 越权回归（审计缺陷 1）：
// 同一个 agent 服务同时服务两条 work 时，站在 w-alice 的调用方不得替 w-bob 开调用。
func TestOpenCallRejectsCrossWork(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w-alice", "alice", "reasoner")
	seedCallsWork(t, st, "w-bob", "bob", "reasoner")
	linkCallsTestTask(t, st, "w-bob", "conv-bob")

	// 身份是 reasoner（两条 work 的 agent 相同），但调用方 work 是 w-alice。
	caller := spv2.AgentRef("reasoner")
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w-bob", Service: "shell"}, "w-alice", caller); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("跨 work call_open 应 permission_denied: %v", err)
	}
	// 拒绝后目标 work 名下不得留下任何 call。
	if n := countRowsIn(t, st, `SELECT COUNT(*) FROM calls WHERE work = 'w-bob'`); n != 0 {
		t.Fatalf("跨 work 拒绝不应落 call，实际 %d 条", n)
	}
	// frame.work 为空同样拒绝（不能退化成「没有调用方就放行」）。
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w-bob", Service: "shell"}, "", caller); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("缺调用方 work 应 permission_denied: %v", err)
	}
	// owner 身份但 owner 不匹配：拒绝；匹配才放行。
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w-bob", Service: "shell"}, "w-alice", spv2.HumanRef("mallory")); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("伪 owner 应 permission_denied: %v", err)
	}
	if _, err := s.openCall(ctx, protocol.CallOpenPayload{Work: "w-bob", Service: "shell"}, "w-bob", spv2.HumanRef("bob")); err != nil {
		t.Fatalf("bob 自己的 owner 入口应放行: %v", err)
	}
}

// countRowsIn 执行一条 COUNT 查询（跨 work 回归里核对“拒绝后零写入”）。
func countRowsIn(t *testing.T, st *store.Store, query string) int {
	t.Helper()
	var n int
	if err := st.DB().QueryRow(query).Scan(&n); err != nil {
		t.Fatalf("查询 %s: %v", query, err)
	}
	return n
}

func TestCallControlPauseUnavailableKeepsState(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, st, "w1", store.CallModeResident, true)

	_, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-pause", Op: "pause"}, "w1", spv2.AgentRef("reasoner"))
	if !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("pause 未接 owner 应 unavailable: %v", err)
	}
	row, err := st.GetCall(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.Observe != store.CallObserveWaiting || row.Exec != store.CallExecCreated || row.Phase != "" {
		t.Fatalf("pause 失败不得改状态: %+v", row)
	}

	// 失败不应留下 control 记录：同 control_id 重试仍是 unavailable，而不是“已完成”。
	if _, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-pause", Op: "pause"}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeUnavailable) {
		t.Fatalf("pause 重试应仍 unavailable: %v", err)
	}

	// resume 只能从 paused 出发；created 直接 resume 是 conflict。
	if _, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-resume-1", Op: "resume"}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("created 上 resume 应 conflict: %v", err)
	}

	// 构造 paused（直接落库，模拟 owner 已确认的既有状态），resume 仍需 owner 握手。
	if err := st.SetCallState(ctx, id, store.CallObserveWaiting, store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallState(ctx, id, store.CallObserveWaiting, store.CallExecPaused, "", "pause_confirmed"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-resume-2", Op: "resume"}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeUnavailable) {
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
	id := openCallsTestCall(t, s, st, "w1", store.CallModeOnce, true)
	first, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-cancel", Op: "cancel"}, "w1", spv2.AgentRef("reasoner"))
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
	replay, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-cancel", Op: "cancel"}, "w1", spv2.AgentRef("reasoner"))
	if err != nil {
		t.Fatalf("同 control_id 重放应成功: %v", err)
	}
	if replay.Observe != first.Observe || replay.Exec != first.Exec || replay.Reason != first.Reason {
		t.Fatalf("重放结果不一致: %+v vs %+v", replay, first)
	}
	// 换个 control_id 再取消已关闭的调用：conflict（不能假装还有效）。
	if _, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id, ControlID: "ctl-cancel-2", Op: "cancel"}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeConflict) {
		t.Fatalf("已关闭调用再取消应 conflict: %v", err)
	}

	// 不可暂停调用：取消后 exec=unknown、不写 phase、remote_may_continue=true。
	id2 := openCallsTestCall(t, s, st, "w1", store.CallModeOnce, false)
	res2, err := s.controlCall(ctx, protocol.CallControlPayload{ID: id2, ControlID: "ctl-cancel-np", Op: "cancel"}, "w1", spv2.AgentRef("reasoner"))
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

func TestCallStatusReportsOwnerHandshakeMissing(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w1", "alice", "reasoner")
	id := openCallsTestCall(t, s, st, "w1", store.CallModeOnce, true)

	res, err := s.callStatus(ctx, protocol.CallStatusPayload{IDs: []string{id, "missing-call"}}, "w1", spv2.AgentRef("reasoner"))
	if err != nil {
		t.Fatal(err)
	}
	if res.OwnerRecoveryComplete {
		t.Fatal("owner 对账未接入，owner_recovery_complete 必须显式 false")
	}
	if len(res.Items) != 2 {
		t.Fatalf("items = %+v", res.Items)
	}
	if res.Items[0].ID != id || res.Items[0].Observe != store.CallObserveWaiting ||
		res.Items[0].Exec != store.CallExecCreated || res.Items[0].OwnerService != "shell" {
		t.Fatalf("已知 call 的对账项不对: %+v", res.Items[0])
	}
	// 查不到的 id 只回 id；不得判成 orphaned（owner 恢复未完成）。
	if res.Items[1].ID != "missing-call" || res.Items[1].Exec != "" || res.Items[1].Phase != "" {
		t.Fatalf("未知 call 的对账项不应被判定: %+v", res.Items[1])
	}

	if _, err := s.callStatus(ctx, protocol.CallStatusPayload{}, "w1", spv2.AgentRef("reasoner")); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("空 ids 应 invalid_argument: %v", err)
	}
}

func TestResidentListFiltersByOwner(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w-alice", "alice", "reasoner")
	seedCallsWork(t, st, "w-bob", "bob", "reasoner-b")
	linkCallsTestTask(t, st, "w-alice", "conv-alice")
	linkCallsTestTask(t, st, "w-bob", "conv-bob")

	a1 := openCallsTestCall(t, s, st, "w-alice", store.CallModeResident, true)
	time.Sleep(time.Millisecond)
	a2 := openCallsTestCall(t, s, st, "w-alice", store.CallModeResident, true)
	_ = openCallsTestCall(t, s, st, "w-alice", store.CallModeOnce, true) // 非常驻不出现
	_ = openCallsTestCall(t, s, st, "w-bob", store.CallModeResident, true)

	res, err := s.residentList(ctx, residentListRequest{Work: "w-alice", Limit: 10}, spv2.ServiceRef("reasoner"))
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
		// 会话 ID 必须是不透明值，不能是原文。
		if item.OriginSession == "" || item.OriginSession == "conv-alice" {
			t.Fatalf("origin_session 应是不透明会话 ID: %q", item.OriginSession)
		}
	}

	// 分页：limit=1 时 next 指向下一页游标。
	page1, err := s.residentList(ctx, residentListRequest{Work: "w-alice", Limit: 1}, spv2.ServiceRef("reasoner"))
	if err != nil || len(page1.Items) != 1 || page1.Items[0].ID != a2 || page1.Next == "" {
		t.Fatalf("第一页 = %+v, err=%v", page1, err)
	}
	page2, err := s.residentList(ctx, residentListRequest{Work: "w-alice", Limit: 1, Cursor: page1.Next}, spv2.ServiceRef("reasoner"))
	if err != nil || len(page2.Items) != 1 || page2.Items[0].ID != a1 {
		t.Fatalf("第二页 = %+v, err=%v", page2, err)
	}

	// 非该 work 的 agent/入口：permission_denied。
	if _, err := s.residentList(ctx, residentListRequest{Work: "w-alice"}, spv2.ServiceRef("intruder")); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("越权调用应 permission_denied: %v", err)
	}
	// 解析不到调用方 work：permission_denied（不是空列表）。
	if _, err := s.residentList(ctx, residentListRequest{}, spv2.ServiceRef("reasoner")); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("缺 work 应 permission_denied: %v", err)
	}
}

func TestImageNoteRedactsTruncatesAndPartitions(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	seedCallsWork(t, st, "w-alice", "alice", "reasoner")
	linkCallsTestTask(t, st, "w-alice", "conv-alice")

	longGist := "看图 https://example.com/secret/path " + strings.Repeat("图", 100)
	res, err := s.imageNote(ctx,
		imageNoteRequest{Work: "w-alice", ImageID: "img-1", Gist: longGist},
		spv2.ServiceRef("reasoner"), "w-alice")
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != store.ImageStatusStored {
		t.Fatalf("默认 status 应为 stored: %+v", res)
	}
	if n := utf8.RuneCountInString(res.Gist); n > 80 {
		t.Fatalf("gist 超 80 字: %d（%q）", n, res.Gist)
	}
	if !strings.Contains(res.Gist, "[链接]") || strings.Contains(res.Gist, "example.com") {
		t.Fatalf("gist 未脱敏: %q", res.Gist)
	}

	row, err := st.GetImageIndex(ctx, "alice", "conv-alice", "img-1")
	if err != nil {
		t.Fatal(err)
	}
	if row.Gist != res.Gist || row.Status != store.ImageStatusStored {
		t.Fatalf("image_index 落库不对: %+v", row)
	}

	// 覆盖写 + lost。
	res2, err := s.imageNote(ctx,
		imageNoteRequest{Work: "w-alice", ImageID: "img-1", Gist: "图丢了", Status: store.ImageStatusLost},
		spv2.ServiceRef("reasoner"), "w-alice")
	if err != nil {
		t.Fatal(err)
	}
	if res2.Status != store.ImageStatusLost || res2.Gist != "图丢了" {
		t.Fatalf("丢失记录不对: %+v", res2)
	}
	if row, _ := st.GetImageIndex(ctx, "alice", "conv-alice", "img-1"); row.Status != store.ImageStatusLost {
		t.Fatalf("覆盖写没生效: %+v", row)
	}

	// 会话隔离：同一图片在别的会话里没有记录。
	if _, err := st.GetImageIndex(ctx, "alice", "conv-other", "img-1"); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatalf("跨会话应 ErrImageNotFound: %v", err)
	}

	// 参数校验。
	if _, err := s.imageNote(ctx, imageNoteRequest{Work: "w-alice"}, spv2.ServiceRef("reasoner"), "w-alice"); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("缺 image_id 应 invalid_argument: %v", err)
	}
	if _, err := s.imageNote(ctx, imageNoteRequest{Work: "w-alice", ImageID: "img-2", Status: "weird"},
		spv2.ServiceRef("reasoner"), "w-alice"); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非法 status 应 invalid_argument: %v", err)
	}
}

func TestImageNoteRejectsCrossOwnerAndUnlinkedWork(t *testing.T) {
	s, st := newCallsTestServer(t)
	ctx := context.Background()
	// 两个 work 的 owner 不同；agent 相同，专门验证 owner 维度而不是 agent 维度。
	seedCallsWork(t, st, "w-alice", "alice", "reasoner")
	seedCallsWork(t, st, "w-bob", "bob", "reasoner")
	linkCallsTestTask(t, st, "w-alice", "conv-alice")
	linkCallsTestTask(t, st, "w-bob", "conv-bob")

	// 调用方在 w-alice（owner=alice），却给 w-bob（owner=bob）写图片要点：拒绝。
	_, err := s.imageNote(ctx,
		imageNoteRequest{Work: "w-bob", ImageID: "img-x", Gist: "越权"},
		spv2.ServiceRef("reasoner"), "w-alice")
	if !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("跨 owner 写入应 permission_denied: %v", err)
	}
	// 拒绝后目标 work 的图片索引不应出现这条记录。
	if _, err := st.GetImageIndex(ctx, "bob", "conv-bob", "img-x"); !errors.Is(err, store.ErrImageNotFound) {
		t.Fatalf("越权写入不应落库: %v", err)
	}

	// 调用方 work 不存在：permission_denied（不能退化成“没有调用方”）。
	if _, err := s.imageNote(ctx, imageNoteRequest{Work: "w-alice", ImageID: "img-y", Gist: "x"},
		spv2.ServiceRef("reasoner"), "w-missing"); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("调用方 work 不存在应 permission_denied: %v", err)
	}

	// work 没关联 task：解析不到会话 → invalid_argument。
	seedCallsWork(t, st, "w-no-task", "alice", "reasoner")
	if _, err := s.imageNote(ctx, imageNoteRequest{Work: "w-no-task", ImageID: "img-z", Gist: "x"},
		spv2.ServiceRef("reasoner"), "w-no-task"); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("未关联 task 应 invalid_argument: %v", err)
	}

	// 非该 work 的 agent/入口：authorize 直接拒绝。
	if _, err := s.imageNote(ctx, imageNoteRequest{Work: "w-alice", ImageID: "img-w", Gist: "x"},
		spv2.ServiceRef("intruder"), "w-alice"); !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("越权调用应 permission_denied: %v", err)
	}
}
