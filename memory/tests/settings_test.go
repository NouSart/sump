// memory 设置项测试：
//   - 声明随名片上报；configure 下发即时生效（recall.history_limit）；
//   - 五个预算键的**行为级**验证：core_limit / core_max_chars / rel_limit / rel_max_chars / history_limit；
//   - 非法值与未声明键被拒、保持旧值。
package tests

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/compose"
	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/protocol"
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

// TestSettingsLive 验证：名片声明 13 项（5 个 recall.* + recall.embedding + 2 个 share.* + 5 个只读）；
// configure 合法值即时生效（recall 只带最近一条历史）；非法值与未声明键被拒且保持旧值。
func TestSettingsLive(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 名片声明（按声明顺序）：5 个 recall.* + recall.embedding + 2 个 share.* + 5 个只读项
	declared := sc.DeclaredSettings("memory")
	if len(declared) != 13 {
		t.Fatalf("名片应声明 13 个设置项: %+v", declared)
	}
	if declared[0].Key != "recall.history_limit" || declared[0].Default != "12" {
		t.Fatalf("recall.history_limit 声明不符: %+v", declared[0])
	}
	if it := declared[5]; it.Key != "recall.embedding" || it.Default != "embedding" || it.ReadOnly {
		t.Fatalf("recall.embedding 声明不符（应为普通项）：%+v", it)
	}
	for _, key := range []string{"db", "name", "addr", "center"} {
		found := false
		for _, it := range declared {
			if it.Key == key {
				found = it.ReadOnly
			}
		}
		if !found {
			t.Fatalf("%s 应是只读项: %+v", key, declared)
		}
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

	// 只读项：改动被明确拒绝（启动参数带起来的，换它要重启）。
	if ro := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "db", Value: "改我试试"}); ro.OK || !strings.Contains(ro.Error, "只读") {
		t.Fatalf("只读项应被拒绝: %+v", ro)
	}

	// recall.embedding 热改：换成另一个名字的服务 → 语义召回真的改叫新服务（行为断言，
	// 不是只读 SettingValue）。前提：有长期记忆条目 + 名册里能看到新名字。
	embB := startStubEmbeddingNamed(t, sc, "embedding-b")
	remember(t, caller, srv, "偏好", "爱吃苹果")
	waitRosterName(t, srv, "embedding-b")
	if emb := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "recall.embedding", Value: "embedding-b", Default: "embedding", Overridden: true}); !emb.OK {
		t.Fatalf("recall.embedding 应可热改: %+v", emb)
	}
	if v := srv.SettingValue("recall.embedding"); v != "embedding-b" {
		t.Fatalf("生效值应为 embedding-b，实际 %q", v)
	}
	recallBlock(t, caller, srv, "苹果偏好") // 触发一次 recall（内部会向 embedding 发 embed）
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(embB.Requests()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if len(embB.Requests()) == 0 {
		t.Fatal("热改后新 embedding 服务应收到 embed 请求")
	}

	// 空值 = 关闭语义召回（静默回落 bigram），改回默认再验一次
	before := len(embB.Requests())
	if emb := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "recall.embedding", Value: ""}); !emb.OK {
		t.Fatalf("清空 embedding 名应被接受: %+v", emb)
	}
	if v := srv.SettingValue("recall.embedding"); v != "" {
		t.Fatalf("清空后生效值应为空，实际 %q", v)
	}
	recallBlock(t, caller, srv, "苹果偏好")
	time.Sleep(300 * time.Millisecond)
	if after := len(embB.Requests()); after != before {
		t.Fatalf("关掉 embedding 后不该再发 embed 请求: %d → %d", before, after)
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

// recallBlock 发起一次 recall（查询词「苹果」）并返回注入的记忆块内容（无块则空串）。
func recallBlock(t *testing.T, caller *stubCaller, srv *server.Server, conv string) string {
	t.Helper()
	task := protocol.RecallPayload{Context: protocol.TaskContext{
		ConversationID: conv,
		Boss:           "stub-boss",
		Messages: []protocol.Message{
			{Role: "system", Content: "sys"},
			{Role: "user", Content: "苹果"},
		},
	}}
	if rp := caller.recallUntilReady(t, srv.WsURL(), task); !rp.OK {
		t.Fatalf("recall 失败: %s", rp.Error)
	}
	select {
	case r := <-caller.resumes:
		for _, m := range r.tc.Messages {
			if m.Role == "system" && strings.HasPrefix(m.Content, compose.MemoryPrefix) {
				return m.Content
			}
		}
		return ""
	case <-time.After(5 * time.Second):
		t.Fatal("超时：未收到 resume")
		return ""
	}
}

// TestRecallBudgetsLive 行为级验证其余四个预算键（cumulative 顺序执行，各步显式断言：
// core_limit=1 条数截断 → rel_limit=1 条数截断 → rel_max_chars=1 相关节整体排除 →
// core_max_chars=1 核心节整体排除）。
func TestRecallBudgetsLive(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12)
	caller := startStubCaller(t, sc.URL())

	// 预置：两条核心（priority 2 / 1）+ 两条命中查询词「苹果」的相关条目（同分 → 新近优先）。
	for _, p := range []protocol.RememberPayload{
		{Kind: "身份", Content: "核心甲高优", Priority: 2},
		{Kind: "身份", Content: "核心乙低优", Priority: 1},
		{Kind: "偏好", Content: "爱吃苹果"},
		{Kind: "偏好", Content: "苹果派好吃"},
	} {
		if rp := caller.call(t, srv.WsURL(), "remember", p); !rp.OK {
			t.Fatalf("remember 失败: %s", rp.Error)
		}
	}

	steps := []struct {
		key, value string
		want       []string // 记忆块中必须出现
		notWant    []string // 记忆块中必须不出现
	}{
		// core_limit=1：核心节只留优先级最高那条；相关节不受影响（两条都在）。
		{"recall.core_limit", "1",
			[]string{"核心甲高优", "苹果派好吃", "爱吃苹果"},
			[]string{"核心乙低优"}},
		// rel_limit=1：相关节只留新近那条（「苹果派好吃」后写入，ID 更大）。
		{"recall.rel_limit", "1",
			[]string{"核心甲高优", "苹果派好吃"},
			[]string{"核心乙低优", "爱吃苹果"}},
		// rel_max_chars=1：预算小于任何条目 → 相关节整体排除。
		{"recall.rel_max_chars", "1",
			[]string{"核心甲高优"},
			[]string{"核心乙低优", "苹果派好吃", "爱吃苹果"}},
		// core_max_chars=1：核心节整体排除；核心条目也不因预算溢出而漏进相关节（不命中查询词）。
		{"recall.core_max_chars", "1",
			nil,
			[]string{"核心甲高优", "核心乙低优", "苹果派好吃", "爱吃苹果"}},
	}
	for _, step := range steps {
		if rp := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: step.key, Value: step.value, Default: "x", Overridden: true}); !rp.OK {
			t.Fatalf("%s=%s 下发失败: %s", step.key, step.value, rp.Error)
		}
		block := recallBlock(t, caller, srv, "budget")
		for _, want := range step.want {
			if !strings.Contains(block, want) {
				t.Fatalf("%s=%s：记忆块缺 %q\n%s", step.key, step.value, want, block)
			}
		}
		for _, notWant := range step.notWant {
			if strings.Contains(block, notWant) {
				t.Fatalf("%s=%s：记忆块不应含 %q\n%s", step.key, step.value, notWant, block)
			}
		}
	}
}
