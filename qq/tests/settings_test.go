// qq 设置项测试：owner 下发即时生效（放宽 / 收紧都即时）；非法值与未声明键被拒、保持旧值。
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// pushConfigure 以设置中心身份向服务地址推送一条 configure（单键）并取回执。
func pushConfigure(t *testing.T, addr string, p protocol.ConfigurePayload) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := protocol.Dial(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	env, err := protocol.NewEnvelope(protocol.TypeJump, "settings-center", "qq", "trace-cfg",
		protocol.JumpPayload{Action: "configure", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// TestSettingsOwnerLive 验证：放宽（新主人可用）与收紧（旧授权立即失效）都即时生效；
// 非法值与未声明键被拒且保持旧值。
func TestSettingsOwnerLive(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	al := startStubReasoner(t, sc)
	_ = startStubImages(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	// 放宽：owner=20002 → 20002 的私聊应触发任务（不再发拒绝语）。
	ok := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "owner", Value: "20002", Default: "2271917353", Overridden: true})
	if !ok.OK {
		t.Fatalf("下发失败: %s", ok.Error)
	}
	if v := qq.SettingValue("owner"); v != "20002" {
		t.Fatalf("生效值应为 20002，实际 %q", v)
	}
	nc.sendEvent(t, privateTextEvent(20002, "你好"))
	req := al.waitRequest(t, 5*time.Second)
	if req.Payload["conversation_id"] != "qq:private:20002" {
		t.Fatalf("会话标识不符: %+v", req.Payload)
	}

	// 收紧：owner=2271917353 → 20002 立即被拒（收到拒绝语）。
	ok = pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "owner", Value: "2271917353"})
	if !ok.OK {
		t.Fatalf("下发失败: %s", ok.Error)
	}
	nc.sendEvent(t, privateTextEvent(20002, "再来"))
	act := nc.waitAction(t, "send_msg", 5*time.Second)
	params, _ := act["params"].(map[string]any)
	if msg, _ := params["message"].(string); !strings.Contains(msg, "授权用户") {
		t.Fatalf("收紧后应拒绝: %+v", params)
	}

	// 非法值 / 未声明键：拒绝且保持旧值。
	bad := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "owner", Value: "abc"})
	if bad.OK || !strings.Contains(bad.Error, "纯数字") {
		t.Fatalf("非法值应被拒绝: %+v", bad)
	}
	if v := qq.SettingValue("owner"); v != "2271917353" {
		t.Fatalf("拒绝后应保持旧值，实际 %q", v)
	}
	ghost := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "ghost", Value: "x"})
	if ghost.OK || !strings.Contains(ghost.Error, "未声明的设置项") {
		t.Fatalf("未声明键应被拒绝: %+v", ghost)
	}
}
