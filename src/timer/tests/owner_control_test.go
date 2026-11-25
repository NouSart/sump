package tests

// owner 侧控制面（R-029）的 timer 行为：status 认识/不认识计时句柄；cancel 复用撤销路径
// （照旧给原调用方发 final 补报）；pause / resume 没有概念 → not_supported。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vmihailenco/msgpack/v5"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	"github.com/NouSart/sump/timer/server"
)

// ownerTimerRequest 以 "memory" 的名义发一次原生 call（owner 控制面的调用方站位）。
func ownerTimerRequest(t *testing.T, c *spv2.Client, method string, in any) (msgpack.RawMessage, error) {
	t.Helper()
	f, err := spv2.NewCall("w-owner", spv2.ServiceRef("memory"), spv2.ServiceRef("timer"), method, in)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, f)
	if err != nil {
		return nil, err
	}
	return msgpack.RawMessage(resp.Body), nil
}

// ownerTimerStatus 发一次 owner_status 并解出结果。
func ownerTimerStatus(t *testing.T, c *spv2.Client, ids ...string) protocol.OwnerStatusResult {
	t.Helper()
	raw, err := ownerTimerRequest(t, c, "owner_status", protocol.OwnerStatusPayload{IDs: ids})
	if err != nil {
		t.Fatalf("owner_status 失败: %v", err)
	}
	var st protocol.OwnerStatusResult
	if err := protocol.DecodeRaw(raw, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

// ownerTimerControl 发一次 owner_control：成功解出回执，失败返回原始错误（错误码可断言）。
func ownerTimerControl(t *testing.T, c *spv2.Client, id, op string) (protocol.OwnerControlResult, error) {
	t.Helper()
	raw, err := ownerTimerRequest(t, c, "owner_control", protocol.OwnerControlPayload{ID: id, Op: op})
	if err != nil {
		return protocol.OwnerControlResult{}, err
	}
	var res protocol.OwnerControlResult
	if err := protocol.DecodeRaw(raw, &res); err != nil {
		t.Fatal(err)
	}
	return res, nil
}

// TestOwnerStatusAndControl：在计时器可对账；cancel 确认并照旧发 final 补报；pause/resume
// 与不认识的句柄都明确失败（不假装成功）。
func TestOwnerStatusAndControl(t *testing.T) {
	sc := startStubCenter(t)
	c := startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	res, _ := sleep(t, cl, "caller", protocol.TimerSleepPayload{MS: 5000, Note: "别等"})

	// status：在计时器 = running；不认识的 id 不出现在 states 里；recovery_complete 显式 true。
	st := ownerTimerStatus(t, cl, res.ItemID, "never-seen")
	if !st.RecoveryComplete {
		t.Fatalf("recovery_complete 应为 true（计时只在内存里，没有待恢复账本）: %+v", st)
	}
	if st.States[res.ItemID] != protocol.OwnerExecRunning {
		t.Fatalf("在计时器的执行维应为 running: %+v", st.States)
	}
	if _, ok := st.States["never-seen"]; ok {
		t.Fatalf("不认识的 id 不该出现在 states 里: %+v", st.States)
	}

	// pause / resume：计时没有这个概念 → not_supported，错误信息指明对象。
	for _, op := range []string{protocol.OwnerOpPause, protocol.OwnerOpResume} {
		_, err := ownerTimerControl(t, cl, res.ItemID, op)
		if !spv2.IsCode(err, spv2.CodeNotSupported) {
			t.Fatalf("%s 应回 not_supported，实际 %v", op, err)
		}
		if !strings.Contains(err.Error(), res.ItemID) {
			t.Fatalf("%s 的错误信息应指明对象 %q: %v", op, res.ItemID, err)
		}
	}

	// cancel：复用撤销路径 → 确认 cancelled，且原调用方照旧收到 final 补报。
	ctl, err := ownerTimerControl(t, cl, res.ItemID, protocol.OwnerOpCancel)
	if err != nil {
		t.Fatalf("cancel 失败: %v", err)
	}
	if ctl.Exec != protocol.OwnerExecCancelled || ctl.Reason != "" {
		t.Fatalf("cancel 应确认 cancelled: %+v", ctl)
	}
	if a := takeReport(t, c, "canceled", res.ItemID); !a.p.Final {
		t.Fatalf("撤销补报应是终态: %+v", a.p)
	}
	noReport(t, c, "done", res.ItemID, 300*time.Millisecond)

	// 撤销后：不再认识这个句柄；重复 cancel / 不认识的 id 都回 not_found。
	if _, ok := ownerTimerStatus(t, cl, res.ItemID).States[res.ItemID]; ok {
		t.Fatal("撤销后不该再认识这个句柄")
	}
	if _, err := ownerTimerControl(t, cl, res.ItemID, protocol.OwnerOpCancel); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("重复 cancel 应回 not_found，实际 %v", err)
	}
	if _, err := ownerTimerControl(t, cl, "never-seen", protocol.OwnerOpCancel); !spv2.IsCode(err, spv2.CodeNotFound) {
		t.Fatalf("不认识的句柄应回 not_found，实际 %v", err)
	}
}
