// memory 设置项测试：声明随名片上报；configure 下发即时生效（recall.history_limit）；
// 非法值与未声明键被拒、保持旧值。
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/memory/compose"
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
	env, err := protocol.NewEnvelope(protocol.TypeJump, "settings-center", "memory", "trace-cfg",
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

// TestSettingsLive 验证：名片声明 5 项；configure 合法值即时生效（recall 只带最近一条历史）；
// 非法值与未声明键被拒且保持旧值。
func TestSettingsLive(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 名片声明（按声明顺序）。
	declared := sc.DeclaredSettings("memory")
	if len(declared) != 5 {
		t.Fatalf("名片应声明 5 个设置项: %+v", declared)
	}
	if declared[0].Key != "recall.history_limit" || declared[0].Default != "12" {
		t.Fatalf("recall.history_limit 声明不符: %+v", declared[0])
	}

	// 预置三条历史。
	for _, m := range []protocol.StorePayload{
		{ConversationID: "t9", Role: "user", Content: "历史一"},
		{ConversationID: "t9", Role: "assistant", Content: "历史二"},
		{ConversationID: "t9", Role: "user", Content: "历史三"},
	} {
		if rp := caller.call(t, srv.WsURL(), "store", m); !rp.OK {
			t.Fatalf("store 失败: %s", rp.Error)
		}
	}

	// 非法值 / 未声明键：拒绝且保持旧值。
	bad := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "recall.history_limit", Value: "-1"})
	if bad.OK || !strings.Contains(bad.Error, "≥0") {
		t.Fatalf("非法值应被拒绝: %+v", bad)
	}
	ghost := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "ghost", Value: "1"})
	if ghost.OK || !strings.Contains(ghost.Error, "未声明的设置项") {
		t.Fatalf("未声明键应被拒绝: %+v", ghost)
	}

	// 合法值：立即生效。
	ok := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "recall.history_limit", Value: "1", Default: "12", Overridden: true})
	if !ok.OK {
		t.Fatalf("下发失败: %s", ok.Error)
	}
	if v := srv.SettingValue("recall.history_limit"); v != "1" {
		t.Fatalf("生效值应为 1，实际 %q", v)
	}

	// recall：只应注入最近一条历史（历史三）。
	task := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: "t9",
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "新一轮"},
		},
	}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	select {
	case r := <-caller.resumes:
		block := ""
		for _, m := range r.tc.Messages {
			if strings.HasPrefix(m.Content, compose.MemoryPrefix) {
				block = m.Content
			}
		}
		if block == "" {
			t.Fatalf("应有记忆块: %+v", r.tc.Messages)
		}
		if !strings.Contains(block, "历史三") || strings.Contains(block, "历史一") || strings.Contains(block, "历史二") {
			t.Fatalf("history_limit=1 应只注入最近一条: %q", block)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到 resume")
	}
}
