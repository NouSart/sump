package tests

// spec §17.3：每个 tool:true method 必须有 schema/参数校验；idempotent 行为要有重复请求测试。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func TestMemoryToolContractAndNegativeInputs(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, srv.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	card, err := c.Hello(ctx, spv2.ServiceRef("tester"), spv2.ServiceRef("memory"))
	if err != nil {
		t.Fatal(err)
	}
	tools := 0
	for _, m := range card.Methods {
		if !m.Tool {
			continue
		}
		tools++
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Fatalf("tool method %s 缺 Input/Output schema: %+v", m.Name, m)
		}
	}
	if tools != 3 {
		t.Fatalf("memory 应只暴露 3 个工具（dig/remember/shared_sessions），实际 %d", tools)
	}

	// 必填参数缺失：明确 invalid_argument，不能静默写一条空记忆。
	if rp := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{}); rp.OK {
		t.Fatal("remember 缺 kind/content 应被拒")
	} else if !strings.Contains(rp.Error, "invalid_argument") {
		t.Fatalf("remember 参数错误码不符: %s", rp.Error)
	}

	// 幂等：同 kind+content 重复写入只落一条。
	first := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "偏好", Content: "红队幂等"})
	if !first.OK {
		t.Fatalf("remember 失败: %s", first.Error)
	}
	var r1, r2 protocol.RememberResult
	if err := protocol.DecodeRaw(first.Data, &r1); err != nil {
		t.Fatal(err)
	}
	second := caller.call(t, srv.WsURL(), "remember", protocol.RememberPayload{Kind: "偏好", Content: "红队幂等"})
	if !second.OK {
		t.Fatalf("重复 remember 失败: %s", second.Error)
	}
	if err := protocol.DecodeRaw(second.Data, &r2); err != nil {
		t.Fatal(err)
	}
	if r2.Created || r2.ID != r1.ID {
		t.Fatalf("重复 remember 应幂等: first=%+v second=%+v", r1, r2)
	}
}
