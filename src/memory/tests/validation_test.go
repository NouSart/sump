package tests

// context/2 的入口校验：没有 work / trigger 就没有可组装的上下文，必须明确报错，
// 而不是回一条空消息链让调用方以为“记忆服务说没记忆”。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestContextRejectsMissingWorkAndTrigger(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	rp := caller.call(t, srv.WsURL(), "context", protocol.ContextRequest{})
	if rp.OK || !strings.Contains(rp.Error, "work 必填") {
		t.Fatalf("空上下文应说明 work 必填: ok=%v err=%q", rp.OK, rp.Error)
	}
	caller.openWork(t, srv.WsURL(), "w-valid", "c-valid", "校验任务")
	rp = caller.call(t, srv.WsURL(), "context", protocol.ContextRequest{Work: "w-valid"})
	if rp.OK || !strings.Contains(rp.Error, "trigger.kind") {
		t.Fatalf("缺 trigger 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
}
