package session

// 授权原语的包内测试（R-023 P4）：不启 WS，直接构造 Server + 真 SQLite。
// 这里只证明判定本身；「四个入口是否真的先授权再落库」由入口测试与
// testing/tests 的三进程红队用例证明。

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/session/store"
	"github.com/NouSart/sump/spv2"
)

func newAuthorizeTestServer(t *testing.T) (*Server, *store.Store) {
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
	return &Server{cfg: Config{Store: st}, logger: log.New(io.Discard, "", 0)}, st
}

// TestAuthorizeTargetWork 覆盖 authorizeTargetWork 的全部分支：
// 空 work / work 不存在 / 匿名 / runtime / owner 精确匹配 / 帧头与载荷不一致 /
// 身份不属于目标 work。
func TestAuthorizeTargetWork(t *testing.T) {
	s, _ := newAuthorizeTestServer(t)
	ctx := context.Background()

	runtimeRef := spv2.AgentRef("ops").WithAuthority(spv2.AuthorityRuntime)
	ownerAlice := spv2.HumanRef("alice")
	reasonerA := spv2.AgentRef("reasoner-a")
	reasonerB := spv2.AgentRef("reasoner-b")
	frontend := spv2.ServiceRef("frontend")

	cases := []struct {
		name       string
		targetWork string
		callerWork string
		from       spv2.Ref
		code       string
		wantID     string
	}{
		{"载荷 work 为空", "  ", "w-alice", reasonerA, spv2.CodeInvalidArgument, ""},
		{"目标 work 不存在", "w-ghost", "w-ghost", reasonerA, spv2.CodeNotFound, ""},
		{"匿名（service）", "w-alice", "w-alice", spv2.Ref{Kind: "service"}, spv2.CodePermissionDenied, ""},
		{"匿名（runtime 也不放行）", "w-bob", "w-bob", spv2.AgentRef("").WithAuthority(spv2.AuthorityRuntime), spv2.CodePermissionDenied, ""},
		{"runtime 跨 work", "w-bob", "w-alice", runtimeRef, "", "w-bob"},
		{"owner 精确匹配", "w-alice", "", ownerAlice, "", "w-alice"},
		{"owner 跨 work 不匹配", "w-bob", "w-bob", ownerAlice, spv2.CodePermissionDenied, ""},
		{"帧头 work 与载荷 work 不一致", "w-bob", "w-alice", reasonerB, spv2.CodePermissionDenied, ""},
		{"身份不属于目标 work", "w-bob", "w-bob", reasonerA, spv2.CodePermissionDenied, ""},
		{"目标 work 的 agent", "w-bob", "w-bob", reasonerB, "", "w-bob"},
		{"目标 work 的 entry", "w-bob", "w-bob", frontend, "", "w-bob"},
		{"普通身份用 owner id 匹配", "w-alice", "w-alice", spv2.ServiceRef("alice"), "", "w-alice"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, err := s.authorizeTargetWork(ctx, tc.targetWork, tc.callerWork, "context_base", tc.from)
			if tc.code != "" {
				if !spv2.IsCode(err, tc.code) {
					t.Fatalf("期望 %s，实际 %v", tc.code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应放行，实际: %v", err)
			}
			if w.ID != tc.wantID {
				t.Fatalf("返回的 work 应为 %s，实际 %s", tc.wantID, w.ID)
			}
		})
	}

	// 越权拒绝不得把目标 work 的归属当探测接口回显。
	_, err := s.authorizeTargetWork(ctx, "w-bob", "w-alice", "store", reasonerA)
	if err == nil {
		t.Fatal("跨 work 必须拒绝")
	}
	for _, leak := range []string{"w-bob", "reasoner-b", "bob"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误文案泄漏目标 work 归属 %q: %q", leak, err.Error())
		}
	}
}

// TestAuthorizeCallOp 覆盖按目标 call 的 work 归属判定：匿名 / work 行缺失 /
// runtime 跨 work / owner 精确匹配 / 普通调用方必须同 work 且身份属于该 work。
func TestAuthorizeCallOp(t *testing.T) {
	s, _ := newAuthorizeTestServer(t)
	ctx := context.Background()
	rowBob := store.CallRow{ID: "call-bob", Work: "w-bob", Service: "shell"}

	runtimeRef := spv2.AgentRef("ops").WithAuthority(spv2.AuthorityRuntime)
	cases := []struct {
		name       string
		row        store.CallRow
		callerWork string
		from       spv2.Ref
		code       string
	}{
		{"匿名", rowBob, "w-bob", spv2.Ref{Kind: "service"}, spv2.CodePermissionDenied},
		{"匿名（runtime）", rowBob, "w-bob", spv2.AgentRef("").WithAuthority(spv2.AuthorityRuntime), spv2.CodePermissionDenied},
		{"目标 work 行缺失（fail-closed）", store.CallRow{ID: "call-x", Work: "w-ghost"}, "w-ghost", spv2.AgentRef("reasoner-a"), spv2.CodePermissionDenied},
		{"runtime 跨 work", rowBob, "w-alice", runtimeRef, ""},
		{"owner 精确匹配", rowBob, "w-alice", spv2.HumanRef("bob"), ""},
		{"owner 跨 work 不匹配", rowBob, "w-bob", spv2.HumanRef("alice"), spv2.CodePermissionDenied},
		{"普通调用方跨 work", rowBob, "w-alice", spv2.AgentRef("reasoner-a"), spv2.CodePermissionDenied},
		{"同 work 但身份不属于", rowBob, "w-bob", spv2.AgentRef("reasoner-a"), spv2.CodePermissionDenied},
		{"同 work 的 agent", rowBob, "w-bob", spv2.AgentRef("reasoner-b"), ""},
		{"同 work 的 entry", rowBob, "w-bob", spv2.ServiceRef("frontend"), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := s.authorizeCallOp(ctx, tc.row, tc.callerWork, tc.from, "call_control")
			if tc.code != "" {
				if !spv2.IsCode(err, tc.code) {
					t.Fatalf("期望 %s，实际 %v", tc.code, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("应放行，实际: %v", err)
			}
		})
	}

	// 错误文案不回显目标 call 的 work / owner / agent。
	err := s.authorizeCallOp(ctx, rowBob, "w-alice", spv2.AgentRef("reasoner-a"), "call_status")
	if err == nil {
		t.Fatal("跨 work 必须拒绝")
	}
	for _, leak := range []string{"w-bob", "reasoner-b", "bob"} {
		if strings.Contains(err.Error(), leak) {
			t.Fatalf("错误文案泄漏目标调用归属 %q: %q", leak, err.Error())
		}
	}
}
