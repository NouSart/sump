package tests

import (
	"context"
	"io"
	"log"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
	"github.com/NouSart/sump/timer/server"
)

// TestTimerNativeSPv2 直接打 timer 的 /spv2，确认 sleep/list/cancel 不再经过 v1 合成信封。
func TestTimerNativeSPv2(t *testing.T) {
	t.Setenv("SUMP_SPV2", "1")
	sc := startStubCenter(t)
	logger := log.New(io.Discard, "", 0)
	s := server.New(server.Config{
		Name: "timer", Listen: "127.0.0.1:0", Center: sc.URL(),
		MaxDuration: time.Minute, MaxPending: 8,
	}, logger)
	defer s.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	addr := strings.TrimSuffix(s.WsURL(), "/ws") + "/spv2"
	c, err := spv2.Dial(ctx, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	call, err := spv2.NewCall("w-timer", spv2.ServiceRef("tester"), spv2.ServiceRef("timer"), "sleep",
		protocol.TimerSleepPayload{MS: 5000, Note: "native"})
	if err != nil {
		t.Fatal(err)
	}
	var sleep protocol.TimerSleepResult
	if err := c.CallDecode(ctx, call, &sleep); err != nil {
		t.Fatal(err)
	}
	if sleep.ItemID == "" || sleep.Phase != "queued" {
		t.Fatalf("sleep = %+v", sleep)
	}

	listCall, err := spv2.NewCall("w-timer", spv2.ServiceRef("tester"), spv2.ServiceRef("timer"), "list", nil)
	if err != nil {
		t.Fatal(err)
	}
	var list protocol.TimerListResult
	if err := c.CallDecode(ctx, listCall, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Timers) != 1 || list.Timers[0].ItemID != sleep.ItemID {
		t.Fatalf("list = %+v", list)
	}

	cancelCall, err := spv2.NewCall("w-timer", spv2.ServiceRef("tester"), spv2.ServiceRef("timer"), "cancel",
		protocol.TimerCancelPayload{ItemID: sleep.ItemID})
	if err != nil {
		t.Fatal(err)
	}
	var canceled protocol.TimerCancelResult
	if err := c.CallDecode(ctx, cancelCall, &canceled); err != nil {
		t.Fatal(err)
	}
	if canceled.Status != "canceled" {
		t.Fatalf("cancel = %+v", canceled)
	}
}
