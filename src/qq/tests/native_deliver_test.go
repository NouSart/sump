package tests

// qq.deliver 的原生 SPv2.1 入口：服务卡声明、body 直达共享实现、成功与错误回执。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// TestNativeDeliver 覆盖 /spv2 上的 deliver：
//   - 服务卡里有 deliver（带 input schema，且不冒充 LLM 工具 / 异步动作）；
//   - body 直接解成 DeliverPayload，成功回执并发到 NapCat；
//   - 空 text 走业务校验，按 invalid_argument 拒绝。
func TestNativeDeliver(t *testing.T) {
	t.Setenv("SUMP_SPV2", "1")
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, strings.TrimSuffix(qq.WsURL(), "/ws")+"/spv2")
	if err != nil {
		t.Fatalf("连接 /spv2 失败: %v", err)
	}
	defer c.Close()

	card, err := c.Hello(ctx, spv2.ServiceRef("reasoner"), spv2.ServiceRef("qq"))
	if err != nil {
		t.Fatalf("hello 失败: %v", err)
	}
	m, ok := card.Method("deliver")
	if !ok {
		t.Fatal("服务卡缺少 deliver method")
	}
	if m.Tool || m.LongRunning {
		t.Fatalf("deliver 不应声明为 LLM 工具或异步动作: tool=%v long_running=%v", m.Tool, m.LongRunning)
	}
	props, _ := m.Input["properties"].(map[string]any)
	if _, ok := props["text"]; !ok {
		t.Fatalf("deliver input schema 缺少 text: %+v", m.Input)
	}
	if _, ok := props["conversation_id"]; !ok {
		t.Fatalf("deliver input schema 缺少 conversation_id: %+v", m.Input)
	}

	call, err := spv2.NewCall("trace-native-deliver", spv2.ServiceRef("reasoner"), spv2.ServiceRef("qq"), "deliver",
		protocol.DeliverPayload{Text: "原生回复", ConversationID: "qq:private:2271917353"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, call); err != nil {
		t.Fatalf("原生 deliver 失败: %v", err)
	}
	act := nc.waitAction(t, "send_msg", 5*time.Second)
	params, _ := act["params"].(map[string]any)
	if params["message"] != "原生回复" || params["message_type"] != "private" {
		t.Fatalf("send_msg 参数不符: %+v", params)
	}

	// 错误路径：空 text 必须在业务校验处被拒。
	bad, err := spv2.NewCall("trace-native-deliver", spv2.ServiceRef("reasoner"), spv2.ServiceRef("qq"), "deliver",
		protocol.DeliverPayload{Text: " ", ConversationID: "qq:private:2271917353"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(ctx, bad); err == nil || !spv2.IsCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("空 text 应以 invalid_argument 拒绝，得到 %v", err)
	}
}
