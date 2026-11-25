package service

// owner_status / owner_control 骨架 hook 的测试：设置后分发、未设置时不注册（not_found）、
// 错误码透传、ttl 用调用方 ctx 约束、非法载荷 fail-closed。

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// fakeOwner 记录骨架调用 hook 的入参，并按字段回放结果 / 错误。
type fakeOwner struct {
	mu sync.Mutex

	statusIDs []string
	states    map[string]string
	complete  bool
	statusErr error

	controlID     string
	controlOp     string
	controlTTL    time.Duration
	controlExec   string
	controlReason string
	controlErr    error
	waitCtx       bool // true = 阻塞到 ctx 结束（用来验证 ttl 由调用方 ctx 约束）
}

func (f *fakeOwner) OwnerStatus(_ context.Context, ids []string) (map[string]string, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusIDs = append([]string(nil), ids...)
	return f.states, f.complete, f.statusErr
}

func (f *fakeOwner) OwnerControl(ctx context.Context, id, op string, ttl time.Duration) (string, string, error) {
	f.mu.Lock()
	f.controlID, f.controlOp, f.controlTTL = id, op, ttl
	wait, err, exec, reason := f.waitCtx, f.controlErr, f.controlExec, f.controlReason
	f.mu.Unlock()
	if wait {
		<-ctx.Done()
		return "", "", ctx.Err()
	}
	return exec, reason, err
}

func (f *fakeOwner) setControlErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.controlErr = err
}

func (f *fakeOwner) statusIDsSnapshot() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.statusIDs...)
}

func (f *fakeOwner) controlSnapshot() (id, op string, ttl time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.controlID, f.controlOp, f.controlTTL
}

// startOwnerPeer 起一个只构建了 SPv2 服务端（不连设置中心）的骨架实例，返回一个直连客户端。
func startOwnerPeer(t *testing.T, name string, h OwnerControl) (*Service, *spv2.Client) {
	t.Helper()
	peer := New(Config{Name: name, SecretFile: "-"})
	if h != nil {
		peer.SetOwnerControl(h)
	}
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(peer.spv2Srv.Handler())
	t.Cleanup(ts.Close)
	c, err := spv2.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return peer, c
}

// ownerCall 以 "memory" 的名义发一次原生 call（内存侧 OwnerClient 的站位），返回成功时的 body。
func ownerCall(t *testing.T, c *spv2.Client, to, method string, in any) (msgpack.RawMessage, error) {
	t.Helper()
	f, err := spv2.NewCall("w-owner", spv2.ServiceRef("memory"), spv2.ServiceRef(to), method, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return msgpack.RawMessage(resp.Body), nil
}

// TestOwnerControlHookDispatch：接入 hook 后分发正确，两者都不是工具。
func TestOwnerControlHookDispatch(t *testing.T) {
	f := &fakeOwner{
		states: map[string]string{
			"c1": protocol.OwnerExecRunning,
			"c2": protocol.OwnerExecPaused,
		},
		complete:    true,
		controlExec: protocol.OwnerExecPaused,
	}
	peer, c := startOwnerPeer(t, "owner", f)

	// 两个 method 在服务卡里，但都不是工具（模型看不见），也不在 provides 里。
	card := peer.spv2Srv.Card()
	for _, name := range []string{"owner_status", "owner_control"} {
		m, ok := card.Method(name)
		if !ok {
			t.Fatalf("服务卡缺少 method %q", name)
		}
		if m.Tool {
			t.Fatalf("%s 不该是工具（管理面不暴露给模型）: %+v", name, m)
		}
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Fatalf("%s 缺少 input/output schema: %+v", name, m)
		}
		for _, p := range peer.cfg.Provides {
			if p.Action == name {
				t.Fatalf("%s 不该进 provides: %+v", name, p)
			}
		}
	}

	raw, err := ownerCall(t, c, "owner", "owner_status", protocol.OwnerStatusPayload{IDs: []string{"c1", "c2", "c3"}})
	if err != nil {
		t.Fatalf("owner_status 调用失败: %v", err)
	}
	var st protocol.OwnerStatusResult
	if err := protocol.DecodeRaw(raw, &st); err != nil {
		t.Fatal(err)
	}
	if !st.RecoveryComplete {
		t.Fatalf("recovery_complete 丢失: %+v", st)
	}
	if len(st.States) != 2 || st.States["c1"] != protocol.OwnerExecRunning || st.States["c2"] != protocol.OwnerExecPaused {
		t.Fatalf("states 不符: %+v", st.States)
	}
	if ids := f.statusIDsSnapshot(); len(ids) != 3 || ids[0] != "c1" || ids[2] != "c3" {
		t.Fatalf("hook 收到的 ids 不符: %v", ids)
	}

	raw, err = ownerCall(t, c, "owner", "owner_control", protocol.OwnerControlPayload{
		ID: "c1", Op: protocol.OwnerOpPause, TTLMS: 1500,
	})
	if err != nil {
		t.Fatalf("owner_control 调用失败: %v", err)
	}
	var ctl protocol.OwnerControlResult
	if err := protocol.DecodeRaw(raw, &ctl); err != nil {
		t.Fatal(err)
	}
	if ctl.ID != "c1" || ctl.Exec != protocol.OwnerExecPaused {
		t.Fatalf("owner_control 回执不符: %+v", ctl)
	}
	id, op, ttl := f.controlSnapshot()
	if id != "c1" || op != protocol.OwnerOpPause || ttl != 1500*time.Millisecond {
		t.Fatalf("hook 收到的控制入参不符: id=%q op=%q ttl=%s", id, op, ttl)
	}
}

// TestOwnerControlNotInstalled：未接 hook = 两个 method 不存在，direct call 得 not_found。
func TestOwnerControlNotInstalled(t *testing.T) {
	peer, c := startOwnerPeer(t, "owner", nil)
	card := peer.spv2Srv.Card()
	for _, name := range []string{"owner_status", "owner_control"} {
		if _, ok := card.Method(name); ok {
			t.Fatalf("未接控制面时不该声明 %q", name)
		}
	}
	if _, err := ownerCall(t, c, "owner", "owner_status", protocol.OwnerStatusPayload{IDs: []string{"c1"}}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("未设置 hook 时 owner_status 应回 not_found，实际 %v", err)
	}
	if _, err := ownerCall(t, c, "owner", "owner_control", protocol.OwnerControlPayload{ID: "c1", Op: protocol.OwnerOpCancel}); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("未设置 hook 时 owner_control 应回 not_found，实际 %v", err)
	}
}

// TestOwnerControlErrorPassThrough：hook 的错误码必须原样到调用方，且错误信息指明对象；
// 普通错误退化成 internal（不冒充业务错误码）。
func TestOwnerControlErrorPassThrough(t *testing.T) {
	f := &fakeOwner{statusErr: spv2.ErrCode(spv2.CodeNotSupported, "没有持久账本", false, nil)}
	_, c := startOwnerPeer(t, "owner", f)

	_, err := ownerCall(t, c, "owner", "owner_status", protocol.OwnerStatusPayload{IDs: []string{"c1"}})
	if !spv2.IsCode(err, spv2.CodeNotSupported) {
		t.Fatalf("hook 的 not_supported 应透传，实际 %v", err)
	}
	if !strings.Contains(err.Error(), "owner_status") || !strings.Contains(err.Error(), "没有持久账本") {
		t.Fatalf("错误信息应指明方法并保留原因: %v", err)
	}

	f.setControlErr(spv2.ErrCode(spv2.CodeNotFound, "没有这个句柄", false, nil))
	_, err = ownerCall(t, c, "owner", "owner_control", protocol.OwnerControlPayload{ID: "c9", Op: protocol.OwnerOpCancel})
	if !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("hook 的 not_found 应透传，实际 %v", err)
	}
	if !strings.Contains(err.Error(), `id="c9"`) {
		t.Fatalf("错误信息应指明对象 id: %v", err)
	}

	f.setControlErr(errors.New("boom"))
	if _, err := ownerCall(t, c, "owner", "owner_control", protocol.OwnerControlPayload{ID: "c1", Op: protocol.OwnerOpPause}); !spv2.IsCode(err, spv2.CodeInternal) {
		t.Fatalf("普通错误应回 internal，实际 %v", err)
	}
}

// TestOwnerControlTTL：ttl_ms 通过 ctx 交给 hook（不自设定时器）；到点回 deadline_exceeded。
func TestOwnerControlTTL(t *testing.T) {
	f := &fakeOwner{waitCtx: true}
	_, c := startOwnerPeer(t, "owner", f)

	start := time.Now()
	_, err := ownerCall(t, c, "owner", "owner_control", protocol.OwnerControlPayload{
		ID: "c1", Op: protocol.OwnerOpPause, TTLMS: 50,
	})
	elapsed := time.Since(start)
	if !spv2.IsCode(err, spv2.CodeDeadlineExceeded) {
		t.Fatalf("ttl 到期应回 deadline_exceeded，实际 %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("ttl=50ms 却等了 %s：超时没有交给调用方 ctx 控制", elapsed)
	}
	if _, _, ttl := f.controlSnapshot(); ttl != 50*time.Millisecond {
		t.Fatalf("hook 应收到 ttl=50ms，实际 %s", ttl)
	}
}

// TestOwnerControlBadPayload：非法载荷 fail-closed（invalid_argument），且不惊动 hook。
func TestOwnerControlBadPayload(t *testing.T) {
	f := &fakeOwner{}
	_, c := startOwnerPeer(t, "owner", f)

	cases := []struct {
		name   string
		method string
		in     any
	}{
		{"status_empty_ids", "owner_status", protocol.OwnerStatusPayload{}},
		{"control_empty_id", "owner_control", protocol.OwnerControlPayload{Op: protocol.OwnerOpPause}},
		{"control_bad_op", "owner_control", protocol.OwnerControlPayload{ID: "c1", Op: "stop"}},
		{"control_negative_ttl", "owner_control", protocol.OwnerControlPayload{ID: "c1", Op: protocol.OwnerOpPause, TTLMS: -1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ownerCall(t, c, "owner", tc.method, tc.in); !spv2.IsCode(err, spv2.CodeInvalidArgument) {
				t.Fatalf("非法载荷应回 invalid_argument，实际 %v", err)
			}
		})
	}
	if ids := f.statusIDsSnapshot(); len(ids) != 0 {
		t.Fatalf("非法 status 载荷不该调 hook: %v", ids)
	}
	if id, _, _ := f.controlSnapshot(); id != "" {
		t.Fatalf("非法 control 载荷不该调 hook: id=%q", id)
	}
}
