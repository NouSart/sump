package session

// 生产 OwnerClient（spv2OwnerClient）的协议级测试：假 owner 用真实 service 骨架 + 真 SPv2
// 连接起一个对端，覆盖成功 / 空 exec / 矛盾 exec（cancel 答 running）/ 超时 / unknown method /
// not_found / not_supported，以及「调试点发上线的是 remote_item_id 而不是 call_id」。
//
// 不依赖外网：对端是本机 127.0.0.1:0 起的真实 SPv2 服务。

import (
	"context"
	"errors"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// wireFakeOwner 是 owner 侧 hook 的可配置实现（被测客户端通过真 SPv2 连接打到它）。
type wireFakeOwner struct {
	mu sync.Mutex

	// owner_status
	states     map[string]string
	complete   bool
	statusErr  error
	statusWait bool
	statusIDs  []string

	// owner_control
	execByOp      map[string]string // op → exec（缺省 = 空串：已确认但未带状态维）
	controlReason string
	controlErr    error
	controlWait   bool
	controlID     string
	controlOp     string
	controlCalls  int
}

func (f *wireFakeOwner) OwnerStatus(ctx context.Context, ids []string) (map[string]string, bool, error) {
	f.mu.Lock()
	f.statusIDs = append([]string(nil), ids...)
	wait, err, states, complete := f.statusWait, f.statusErr, f.states, f.complete
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return nil, false, ctx.Err()
	}
	return states, complete, err
}

func (f *wireFakeOwner) OwnerControl(ctx context.Context, id, op string, _ time.Duration) (string, string, error) {
	f.mu.Lock()
	f.controlCalls++
	f.controlID, f.controlOp = id, op
	wait, err, reason := f.controlWait, f.controlErr, f.controlReason
	exec := f.execByOp[op]
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	if err != nil {
		return "", reason, err
	}
	return exec, reason, nil
}

func (f *wireFakeOwner) lastStatusIDs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusIDs...)
}

func (f *wireFakeOwner) lastControl() (id, op string, calls int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.controlID, f.controlOp, f.controlCalls
}

// startWireOwner 起一个真实 SPv2 服务端的假 owner；h 为 nil 时两个 method 都不注册
// （用来覆盖 unknown method 的 not_found）。
func startWireOwner(t *testing.T, name string, h service.OwnerControl) string {
	t.Helper()
	peer := service.New(service.Config{
		Name: name, Listen: "127.0.0.1:0", Standalone: true, SecretFile: "-",
		Logger: log.New(io.Discard, "", 0),
	})
	if h != nil {
		peer.SetOwnerControl(h)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := peer.Start(ctx); err != nil {
		t.Fatalf("启动假 owner %s: %v", name, err)
	}
	t.Cleanup(peer.Shutdown)
	return peer.WsURL()
}

// newWireOwnerSession 建一个未 Start 的 session 实例：出站一跳只靠静态 Peers 解析，
// 不需要本服务监听；同时走 main.go 的注入路径（New → SetOwnerClient）。
func newWireOwnerSession(t *testing.T, ownerName, ownerURL string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "memory.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	s := New(Config{
		Store: st, Peers: map[string]string{ownerName: ownerURL},
		Logger: log.New(io.Discard, "", 0),
	}, log.New(io.Discard, "", 0))
	s.SetOwnerClient(NewSPv2OwnerClient(s.Service))
	return s, st
}

// TestSPv2OwnerClientStatus：states 只回本批发出的句柄；owner 多回的 / 空 exec 的键不收。
func TestSPv2OwnerClientStatus(t *testing.T) {
	fake := &wireFakeOwner{
		states: map[string]string{
			"h1": protocol.OwnerExecRunning,
			"h2": protocol.OwnerExecPaused,
			"h9": protocol.OwnerExecDone, // 没问它：不能混进结果
			"h3": "",                     // 空 exec：不是状态，不能带回来
		},
		complete: true,
	}
	url := startWireOwner(t, "fakeowner", fake)
	s, _ := newWireOwnerSession(t, "fakeowner", url)

	states, complete, err := s.owner().Status(context.Background(), "fakeowner", []string{"h1", "h2", "h3"})
	if err != nil {
		t.Fatalf("owner_status 成功路径不应报错: %v", err)
	}
	if !complete {
		t.Fatal("recovery_complete=true 应原样带回")
	}
	if len(states) != 2 || states["h1"] != protocol.OwnerExecRunning || states["h2"] != protocol.OwnerExecPaused {
		t.Fatalf("states 只应包含本批的已知句柄: %+v", states)
	}
	if ids := fake.lastStatusIDs(); len(ids) != 3 || ids[0] != "h1" || ids[2] != "h3" {
		t.Fatalf("owner 收到的 ids 不符: %v", ids)
	}
}

// TestSPv2OwnerClientStatusUnknownMethod：没实现 owner_status 的 owner 回 not_found，
// 客户端必须报错（带方法名），不能退化成「空 states + 成功」。
func TestSPv2OwnerClientStatusUnknownMethod(t *testing.T) {
	url := startWireOwner(t, "plainowner", nil)
	s, _ := newWireOwnerSession(t, "plainowner", url)

	states, complete, err := s.owner().Status(context.Background(), "plainowner", []string{"h1"})
	if err == nil {
		t.Fatalf("unknown method 必须报错，不能假成功（states=%v complete=%v）", states, complete)
	}
	if !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("unknown method 应回 not_found，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "owner_status") || !strings.Contains(err.Error(), "plainowner") {
		t.Fatalf("错误信息应指明 owner 与方法: %v", err)
	}
}

// TestSPv2OwnerClientControlEmptyExec：空 exec = 已确认但未带状态维，原样带回（判定在调用点）。
func TestSPv2OwnerClientControlEmptyExec(t *testing.T) {
	fake := &wireFakeOwner{}
	url := startWireOwner(t, "fakeowner", fake)
	s, _ := newWireOwnerSession(t, "fakeowner", url)

	exec, err := s.owner().Pause(context.Background(), "fakeowner", "remote-1")
	if err != nil {
		t.Fatalf("空 exec 是成功回执，不应报错: %v", err)
	}
	if exec != "" {
		t.Fatalf("空 exec 应原样带回，实际 %q", exec)
	}
	if id, op, calls := fake.lastControl(); id != "remote-1" || op != protocol.OwnerOpPause || calls != 1 {
		t.Fatalf("owner 收到的控制入参不符: id=%q op=%q calls=%d", id, op, calls)
	}
}

// TestSPv2OwnerClientControlContradiction：owner 答还在跑时客户端不加工，调用点按未确认处理。
func TestSPv2OwnerClientControlContradiction(t *testing.T) {
	fake := &wireFakeOwner{execByOp: map[string]string{protocol.OwnerOpCancel: protocol.OwnerExecRunning}}
	url := startWireOwner(t, "fakeowner", fake)
	s, _ := newWireOwnerSession(t, "fakeowner", url)

	exec, err := s.owner().Cancel(context.Background(), "fakeowner", "remote-1")
	if err != nil {
		t.Fatalf("矛盾 exec 是可观测结果，不应变成错误: %v", err)
	}
	if exec != protocol.OwnerExecRunning {
		t.Fatalf("客户端不得把 running 改写成 cancelled，实际 %q", exec)
	}
}

// TestSPv2OwnerClientControlNotFound：owner 不认识句柄 → not_found（带服务与对象），不假成功。
func TestSPv2OwnerClientControlNotFound(t *testing.T) {
	fake := &wireFakeOwner{controlErr: spv2.ErrCode(spv2.CodeNotFound,
		"没有这个受理句柄: remote-9", false, nil)}
	url := startWireOwner(t, "fakeowner", fake)
	s, _ := newWireOwnerSession(t, "fakeowner", url)

	_, err := s.owner().Cancel(context.Background(), "fakeowner", "remote-9")
	if !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("owner 的 not_found 应保留错误码，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "fakeowner") || !strings.Contains(err.Error(), "remote-9") {
		t.Fatalf("错误信息应带服务与对象: %v", err)
	}
}

// TestSPv2OwnerClientControlNotSupported：owner 认识句柄但没有该操作路径 → not_supported + 原因。
func TestSPv2OwnerClientControlNotSupported(t *testing.T) {
	fake := &wireFakeOwner{controlErr: spv2.ErrCode(spv2.CodeNotSupported,
		"shell 没有终止执行中命令的路径", false, nil)}
	url := startWireOwner(t, "fakeowner", fake)
	s, _ := newWireOwnerSession(t, "fakeowner", url)

	_, err := s.owner().Cancel(context.Background(), "fakeowner", "remote-1")
	if !spv2.IsCode(err, spv2.CodeNotSupported) {
		t.Fatalf("owner 的 not_supported 应保留错误码，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "没有终止") || !strings.Contains(err.Error(), "remote-1") {
		t.Fatalf("错误信息应保留原因并带对象: %v", err)
	}
}

// TestSPv2OwnerClientTimeout：ctx 超时必须返回错误（errors.Is 可判定），不得吞成 nil 成功。
func TestSPv2OwnerClientTimeout(t *testing.T) {
	fake := &wireFakeOwner{controlWait: true, statusWait: true}
	url := startWireOwner(t, "fakeowner", fake)
	s, _ := newWireOwnerSession(t, "fakeowner", url)

	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := s.owner().Pause(ctx, "fakeowner", "remote-1")
	if err == nil {
		t.Fatal("控制超时必须报错，不能吞成 nil")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("控制超时应可用 errors.Is(context.DeadlineExceeded) 判定，实际 %v", err)
	}
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("超时未按 ctx 返回（等了 %v）", elapsed)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	if _, _, err := s.owner().Status(ctx2, "fakeowner", []string{"h1"}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("对账超时也应可用 errors.Is 判定，实际 %v", err)
	}
}

// openCallForService 按指定 owner service 开一条 call（openCallsTestCall 写死 shell，
// 这里要让 call 的 service 与假 owner 的名字对上）。
func openCallForService(t *testing.T, s *Server, work, service string, pausable bool, execDeadlineMS int64) string {
	t.Helper()
	w, err := s.cfg.Store.GetWork(context.Background(), work)
	if err != nil {
		t.Fatalf("读取 work %s: %v", work, err)
	}
	res, err := s.openCall(context.Background(), callOpenBody(t, protocol.CallOpenPayload{
		Work: work, Service: service, Action: "run", Mode: store.CallModeOnce,
		Pausable: pausable, TTLMS: 60000, ExecDeadlineMS: execDeadlineMS,
	}), caller{work: work, from: spv2.AgentRef(w.Agent.ID)})
	if err != nil {
		t.Fatalf("openCall(%s/%s): %v", work, service, err)
	}
	return res.CallID
}

// TestSPv2OwnerClientThroughStateMachine：生产客户端接进 session 后的落点语义与 fake 契约一致：
// cancel 时 owner 答 running → unknown + remote_may_continue；pause 确认 → paused。
// 同时证明发上线的是 remote_item_id（不是 call_id）。
func TestSPv2OwnerClientThroughStateMachine(t *testing.T) {
	fake := &wireFakeOwner{execByOp: map[string]string{
		protocol.OwnerOpPause:  protocol.OwnerExecPaused,
		protocol.OwnerOpCancel: protocol.OwnerExecRunning,
	}}
	url := startWireOwner(t, "fakeowner", fake)
	s, st := newWireOwnerSession(t, "fakeowner", url)
	seedCallsWork(t, st, "w1", "alice", "reasoner")

	// cancel：owner 答「还在跑」→ 未确认，不能写 cancelled。
	cancelID := openCallForService(t, s, "w1", "fakeowner", true, 300000)
	if err := st.SetCallState(context.Background(), cancelID, store.CallObserveWaiting,
		store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallRemote(context.Background(), cancelID, "remote-cancel"); err != nil {
		t.Fatal(err)
	}
	res, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: cancelID, ControlID: "ctl-cancel-wire", Op: "cancel",
	})
	if err != nil {
		t.Fatalf("owner 的回答是可观测结果，不应变协议错误: %v", err)
	}
	if res.Observe != store.CallObserveClosed || res.Exec != store.CallExecUnknown || res.Reason != "cancel_unconfirmed" {
		t.Fatalf("owner 答 running 时 cancel 结果不对: %+v", res)
	}
	if id, op, _ := fake.lastControl(); id != "remote-cancel" || op != protocol.OwnerOpCancel {
		t.Fatalf("上线id必须是 remote_item_id: id=%q op=%q", id, op)
	}

	// pause：owner 确认 paused → 落 paused。
	pauseID := openCallForService(t, s, "w1", "fakeowner", true, 300000)
	if err := st.SetCallState(context.Background(), pauseID, store.CallObserveWaiting,
		store.CallExecRunning, "", "dispatched"); err != nil {
		t.Fatal(err)
	}
	if err := st.SetCallRemote(context.Background(), pauseID, "remote-pause"); err != nil {
		t.Fatal(err)
	}
	paused, err := controlCalls(t, s, protocol.CallControlPayload{
		ID: pauseID, ControlID: "ctl-pause-wire", Op: "pause",
	})
	if err != nil || paused.Exec != store.CallExecPaused || paused.Reason != "pause_confirmed" {
		t.Fatalf("pause 确认结果不对: %+v err=%v", paused, err)
	}
	if id, op, _ := fake.lastControl(); id != "remote-pause" || op != protocol.OwnerOpPause {
		t.Fatalf("pause 上线id必须是 remote_item_id: id=%q op=%q", id, op)
	}
}
