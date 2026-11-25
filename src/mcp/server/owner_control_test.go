package server

// owner 控制面（R-029）的 mcp 落点：status 认对象、cancel 只撤本地等待、
// pause/resume 不假装支持；未接线时两个 method 不存在（direct call 得 not_found）。

import (
	"context"
	"io"
	"log"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/spv2"
)

// TestOwnerStatusKnownUnknownAndTerminal：认识的句柄给执行维，终态按实际落点回，
// 不认识的不出现在 states；recovery_complete 恒 true（导入不持久化）。
func TestOwnerStatusKnownUnknownAndTerminal(t *testing.T) {
	s := startGateway(t, Config{}, nil, `{}`)
	s.importExecs.accepted("h-running")
	s.importExecs.finished("h-done", protocol.OwnerExecDone)
	s.importExecs.finished("h-failed", protocol.OwnerExecFailed)
	s.importExecs.accepted("h-cancelled")
	s.importExecs.cancelled("h-cancelled")
	s.importExecs.finished("h-unknown", protocol.OwnerExecUnknown)
	// 取消已生效之后 worker 才落终态：本地等待已被撤销，不能被改写成 done。
	s.importExecs.accepted("h-race")
	s.importExecs.cancelled("h-race")
	s.importExecs.finished("h-race", protocol.OwnerExecDone)

	states, complete, err := s.OwnerStatus(context.Background(), []string{
		"h-running", "h-done", "h-failed", "h-cancelled", "h-unknown", "h-race", "h-missing",
	})
	if err != nil {
		t.Fatalf("owner_status 不应报错: %v", err)
	}
	if !complete {
		t.Fatal("mcp 不持久化导入受理记录，recovery_complete 应恒为 true")
	}
	want := map[string]string{
		"h-running":   protocol.OwnerExecRunning,
		"h-done":      protocol.OwnerExecDone,
		"h-failed":    protocol.OwnerExecFailed,
		"h-cancelled": protocol.OwnerExecCancelled,
		"h-unknown":   protocol.OwnerExecUnknown,
		"h-race":      protocol.OwnerExecCancelled,
	}
	if len(states) != len(want) {
		t.Fatalf("states = %v, want %v", states, want)
	}
	for id, exec := range want {
		if states[id] != exec {
			t.Fatalf("states[%q] = %q, want %q（全部: %v）", id, states[id], exec, states)
		}
	}
	if _, ok := states["h-missing"]; ok {
		t.Fatalf("不认识的句柄不应出现在 states: %v", states)
	}
}

// TestOwnerMethodsOverSPv2：接线后两个 method 真的在 /spv2 上，错误码原样透传。
func TestOwnerMethodsOverSPv2(t *testing.T) {
	s := startGateway(t, Config{}, nil, `{}`)
	s.importExecs.accepted("h-running")
	s.importExecs.finished("h-done", protocol.OwnerExecDone)
	c := dialNativeMCP(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	statusCall, _ := spv2.NewCall("work-owner-status", spv2.ServiceRef("memory"), spv2.ServiceRef("mcp"),
		"owner_status", protocol.OwnerStatusPayload{IDs: []string{"h-running", "h-done", "h-missing"}})
	var status protocol.OwnerStatusResult
	if err := c.CallDecode(ctx, statusCall, &status); err != nil {
		t.Fatalf("owner_status 调用失败: %v", err)
	}
	if !status.RecoveryComplete || status.States["h-running"] != protocol.OwnerExecRunning ||
		status.States["h-done"] != protocol.OwnerExecDone || len(status.States) != 2 {
		t.Fatalf("owner_status 结果错误: %+v", status)
	}

	// 不认识的 id：先认对象 → not_found，不是静默成功。
	missingCall, _ := spv2.NewCall("work-owner-missing", spv2.ServiceRef("memory"), spv2.ServiceRef("mcp"),
		"owner_control", protocol.OwnerControlPayload{ID: "h-missing", Op: protocol.OwnerOpCancel})
	var out protocol.OwnerControlResult
	if err := c.CallDecode(ctx, missingCall, &out); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("不认识的句柄应回 not_found，实际 %v", err)
	}

	// 认识的句柄：pause / resume 回 not_supported，不假装成功。
	for _, op := range []string{protocol.OwnerOpPause, protocol.OwnerOpResume} {
		call, _ := spv2.NewCall("work-owner-"+op, spv2.ServiceRef("memory"), spv2.ServiceRef("mcp"),
			"owner_control", protocol.OwnerControlPayload{ID: "h-running", Op: op})
		if err := c.CallDecode(ctx, call, &out); !spv2.IsCode(err, spv2.CodeNotSupported) {
			t.Fatalf("%s 应回 not_supported，实际 %v", op, err)
		}
	}

	// 已结束的句柄没有可取消的等待：如实回终态（done），不用 cancelled 换确认。
	doneCall, _ := spv2.NewCall("work-owner-done", spv2.ServiceRef("memory"), spv2.ServiceRef("mcp"),
		"owner_control", protocol.OwnerControlPayload{ID: "h-done", Op: protocol.OwnerOpCancel})
	if err := c.CallDecode(ctx, doneCall, &out); err != nil {
		t.Fatalf("cancel 已结束句柄不应报错: %v", err)
	}
	if out.ID != "h-done" || out.Exec != protocol.OwnerExecDone || out.Reason == "" {
		t.Fatalf("已结束句柄的 cancel 回执错误: %+v", out)
	}
}

// TestOwnerControlCancelOnlyLocalWait：两条本地取消路径（owner_control / cancel_import）
// 都只撤本地等待，回 cancelled + outcome_unknown，且 owner_status 能看到 cancelled。
func TestOwnerControlCancelOnlyLocalWait(t *testing.T) {
	s := startGateway(t, Config{}, nil, `{}`)
	c := dialNativeMCP(t, s)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	ownerCtx, ownerCancel := context.WithCancel(context.Background())
	if !s.asyncImports.add("h-owner", "reasoner", ownerCancel) {
		t.Fatal("异步导入登记失败")
	}
	defer s.asyncImports.finish("h-owner")
	s.importExecs.accepted("h-owner")

	exec, reason, err := s.OwnerControl(ctx, "h-owner", protocol.OwnerOpCancel, 0)
	if err != nil {
		t.Fatalf("owner_control cancel 失败: %v", err)
	}
	if exec != protocol.OwnerExecCancelled || reason != importCancelReason {
		t.Fatalf("cancel 回执 = (%q, %q), want (cancelled, outcome_unknown)", exec, reason)
	}
	if ownerCtx.Err() == nil {
		t.Fatal("owner_control cancel 应撤销本地等待")
	}

	nativeCtx, nativeCancel := context.WithCancel(context.Background())
	if !s.asyncImports.add("h-native", "reasoner", nativeCancel) {
		t.Fatal("异步导入登记失败")
	}
	defer s.asyncImports.finish("h-native")
	s.importExecs.accepted("h-native")

	nativeCall, _ := spv2.NewCall("work-cancel-import", spv2.ServiceRef("reasoner"), spv2.ServiceRef("mcp"),
		"cancel_import", map[string]any{"item_id": "h-native"})
	var nativeOut map[string]any
	if err := c.CallDecode(ctx, nativeCall, &nativeOut); err != nil {
		t.Fatalf("cancel_import 调用失败: %v", err)
	}
	if nativeOut["status"] != "observation_cancelled" || nativeOut["remote_outcome"] != "unknown" {
		t.Fatalf("cancel_import 回执错误: %#v", nativeOut)
	}
	if nativeCtx.Err() == nil {
		t.Fatal("cancel_import 应撤销本地等待")
	}

	statusCall, _ := spv2.NewCall("work-owner-after-cancel", spv2.ServiceRef("memory"), spv2.ServiceRef("mcp"),
		"owner_status", protocol.OwnerStatusPayload{IDs: []string{"h-owner", "h-native"}})
	var status protocol.OwnerStatusResult
	if err := c.CallDecode(ctx, statusCall, &status); err != nil {
		t.Fatalf("owner_status 调用失败: %v", err)
	}
	for _, id := range []string{"h-owner", "h-native"} {
		if status.States[id] != protocol.OwnerExecCancelled {
			t.Fatalf("取消后 states[%q] = %q, want cancelled（全部: %v）", id, status.States[id], status.States)
		}
	}
}

// TestOwnerMethodsAbsentWithoutHook：没接控制面（SetOwnerControl 未被调用）时两个 method
// 不注册，direct call 得 not_found。mcp 的接线是唯一注册点，删掉它必须在这里退化成不存在。
func TestOwnerMethodsAbsentWithoutHook(t *testing.T) {
	peer := service.New(service.Config{
		Name: "plainmcp", Listen: "127.0.0.1:0", Standalone: true, SecretFile: "-",
		Logger: log.New(io.Discard, "", 0),
	})
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if err := peer.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(peer.Shutdown)
	c, err := spv2.Dial(ctx, peer.WsURL())
	if err != nil {
		t.Fatalf("连接 /spv2 失败: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	var out map[string]any
	statusCall, _ := spv2.NewCall("work-plain-status", spv2.ServiceRef("memory"), spv2.ServiceRef("plainmcp"),
		"owner_status", protocol.OwnerStatusPayload{IDs: []string{"h1"}})
	if err := c.CallDecode(ctx, statusCall, &out); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("未接线时 owner_status 应回 not_found，实际 %v", err)
	}
	controlCall, _ := spv2.NewCall("work-plain-control", spv2.ServiceRef("memory"), spv2.ServiceRef("plainmcp"),
		"owner_control", protocol.OwnerControlPayload{ID: "h1", Op: protocol.OwnerOpCancel})
	if err := c.CallDecode(ctx, controlCall, &out); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("未接线时 owner_control 应回 not_found，实际 %v", err)
	}
}
