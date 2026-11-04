package tests

// timer.cancel 的入口校验：没有句柄就不知道该撤哪条，必须明确拒绝。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/timer/server"
)

func TestCancelRequiresItemID(t *testing.T) {
	sc := startStubCenter(t)
	startCaller(t, sc, "caller")
	startTimer(t, sc, server.Config{})
	cl := dial(t, "ws://"+listenAddr(t, sc))

	rp := request(t, cl, "caller", "cancel", protocol.TimerCancelPayload{})
	if rp.OK || !strings.Contains(rp.Error, "item_id 必填") {
		t.Fatalf("空 item_id 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
}
