package tests

// memory.recall 的入口校验：没有消息就没有可组装的上下文，必须明确报错，
// 而不是回一条空 resume 让调用方以为“记忆服务说没记忆”。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestRecallRejectsEmptyContext(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	rp := caller.call(t, srv.WsURL(), "recall", protocol.RecallPayload{})
	if rp.OK || !strings.Contains(rp.Error, "messages 为空") {
		t.Fatalf("空上下文应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
}
