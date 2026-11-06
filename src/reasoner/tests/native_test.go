package tests

// 原生 SPv2.1 method 的验收：直接打 /spv2，确认 9 个动作走原生 handler，
// 且 boss 语义由 Frame.From.ID 承接。

import (
	"context"
	"log"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/reasoner/server"
	"github.com/NouSart/sump/spv2"
)

// nativeCall 构造一次从 from 到 reasoner 的原生 call（work 固定，便于看 trace）。
func nativeCall(t *testing.T, from, method string, body any) spv2.Frame {
	t.Helper()
	f, err := spv2.NewCall("w-native", spv2.ServiceRef(from), spv2.ServiceRef("reasoner"), method, body)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// dialNative 连上 reasoner 的 /spv2（WsURL 现在已以 /spv2 结尾）。
func dialNative(t *testing.T, s *server.Server) *spv2.Client {
	t.Helper()
	c, err := spv2.Dial(context.Background(), s.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// TestReasonerNativeMethods 校验原生 method 声明与不依赖 v1 动作的分支。
func TestReasonerNativeMethods(t *testing.T) {
	sc := startStubCenter(t)
	s := startReasoner(t, sc.URL())

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c := dialNative(t, s)

	card, err := c.Hello(ctx, spv2.ServiceRef("tester"), spv2.ServiceRef("reasoner"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"echo", "debug_jump", "think", "user_message", "step",
		"resume", "report_result", "user_hold", "retry",
	} {
		m, ok := card.Method(name)
		if !ok {
			t.Fatalf("服务卡缺少原生 method %q: %+v", name, card.Methods)
		}
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Fatalf("method %q 缺 Input/Output JSON Schema: %+v", name, m)
		}
		// 工具标志与 Provides 对齐：reasoner 的动作都不是模型工具。
		if m.Tool {
			t.Fatalf("method %q 不应暴露为模型工具", name)
		}
	}

	// echo：原生 body 直接解码。
	var echo struct {
		Who  string `msgpack:"who"`
		Echo struct {
			Msg string `msgpack:"msg"`
		} `msgpack:"echo"`
	}
	if err := c.CallDecode(ctx, nativeCall(t, "tester", "echo", map[string]any{"msg": "hi"}), &echo); err != nil {
		t.Fatal(err)
	}
	if echo.Who != "reasoner" || echo.Echo.Msg != "hi" {
		t.Fatalf("echo 结果不符: %+v", echo)
	}

	// debug_jump to=self 再回 echo：原生出口 + 原生入口。
	var jumped struct {
		Who  string `msgpack:"who"`
		Echo struct {
			N int `msgpack:"n"`
		} `msgpack:"echo"`
	}
	body := map[string]any{"to": "self", "action": "echo", "input": map[string]any{"n": 42}}
	if err := c.CallDecode(ctx, nativeCall(t, "tester", "debug_jump", body), &jumped); err != nil {
		t.Fatal(err)
	}
	if jumped.Who != "reasoner" || jumped.Echo.N != 42 {
		t.Fatalf("debug_jump 结果不符: %+v", jumped)
	}

	// user_hold：控制面动作。
	var held struct {
		Status string `msgpack:"status"`
		Level  string `msgpack:"level"`
	}
	hold := map[string]any{"conversation_id": "c-native", "hold": "primary"}
	if err := c.CallDecode(ctx, nativeCall(t, "tester", "user_hold", hold), &held); err != nil {
		t.Fatal(err)
	}
	if held.Status != "accepted" || held.Level != "primary" {
		t.Fatalf("user_hold 结果不符: %+v", held)
	}

	// step：没有进行中的任务时忽略，而不是报错。
	var stepped struct {
		Status string `msgpack:"status"`
	}
	if err := c.CallDecode(ctx, nativeCall(t, "tester", "step", map[string]any{"conversation_id": "nope"}), &stepped); err != nil {
		t.Fatal(err)
	}
	if stepped.Status != "ignored" {
		t.Fatalf("step 结果不符: %+v", stepped)
	}

	// report_result：没有在途受理 → ignored。
	var reported struct {
		Status string `msgpack:"status"`
	}
	report := protocol.AsyncResultPayload{ItemID: "item-native", Phase: "done", Final: true}
	if err := c.CallDecode(ctx, nativeCall(t, "tester", "report_result", report), &reported); err != nil {
		t.Fatal(err)
	}
	if reported.Status != "ignored" {
		t.Fatalf("report_result 结果不符: %+v", reported)
	}

	// retry / resume：来源校验走原生错误码（v1 桥只会给 internal，能区分说明打到了原生 handler）。
	_, err = c.Call(ctx, nativeCall(t, "tester", "retry", protocol.RetryPayload{ConversationID: "c-native"}))
	if !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("retry 非门户来源应回 permission_denied，得到 %v", err)
	}
	_, err = c.Call(ctx, nativeCall(t, "tester", "resume", protocol.ResumePayload{Context: protocol.TaskContext{
		Messages: []protocol.Message{{Role: "user", Content: "x"}},
	}}))
	if !spv2.IsCode(err, spv2.CodePermissionDenied) {
		t.Fatalf("resume 非记忆来源应回 permission_denied，得到 %v", err)
	}

	// think：没有 LLM 客户端时如实报错（原生路径同样不 panic）。
	_, err = c.Call(ctx, nativeCall(t, "tester", "think", protocol.ThinkPayload{Prompt: "hi"}))
	if err == nil || !strings.Contains(err.Error(), "LLM 未配置") {
		t.Fatalf("think 未配置 LLM 时应报错，得到 %v", err)
	}

	// resume：from=memory 时接受（没有 LLM 就停在黑板里，不报错）。
	var resumed struct {
		Status string `msgpack:"status"`
	}
	resume := protocol.ResumePayload{Context: protocol.TaskContext{
		ConversationID: "c-native-resume",
		Messages:       []protocol.Message{{Role: "system", Content: "s"}, {Role: "user", Content: "x"}},
	}}
	if err := c.CallDecode(ctx, nativeCall(t, "memory", "resume", resume), &resumed); err != nil {
		t.Fatal(err)
	}
	if resumed.Status != "accepted" {
		t.Fatalf("resume 结果不符: %+v", resumed)
	}
}

// TestReasonerNativeUserMessageBoss 验证原生 user_message 的 boss 来源：
// SPv2 没有 boss 字段，Frame.From.ID 就是结果的归属方——最终 deliver 必须回到它，
// 且 trace 原样沿链（Frame.Work）。
func TestReasonerNativeUserMessageBoss(t *testing.T) {
	sc := startStubCenter(t)
	boss := startStubBoss(t, sc)
	mem := startStubMemory(t, sc)

	fake := &llm.Fake{Replies: []llm.Message{{Role: "assistant", Content: "原生答复"}}}
	logger := log.New(os.Stdout, "[native-chain] ", log.LstdFlags)
	s := server.New(server.Config{
		Name:              "reasoner",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		Memory:            "memory",
		LLM:               fake,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	mem.setReasonerURL(s.WsURL())

	c := dialNative(t, s)
	// 原生 call 的 from 是 stub-boss：它既是入口，也是结果归属方。
	f, err := spv2.NewCall("w-native-chain", spv2.ServiceRef("stub-boss"), spv2.ServiceRef("reasoner"), "user_message",
		protocol.UserMessagePayload{Text: "你好"})
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		Status string `msgpack:"status"`
	}
	if err := c.CallDecode(ctx, f, &ack); err != nil {
		t.Fatal(err)
	}
	if ack.Status != "accepted" {
		t.Fatalf("user_message 未被受理: %+v", ack)
	}

	select {
	case d := <-boss.delivered:
		if d.Text != "原生答复" {
			t.Fatalf("交付内容不符: %q", d.Text)
		}
		if d.ConversationID != "default" {
			t.Fatalf("交付应带回会话标识: %q", d.ConversationID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("超时：boss 未收到 deliver（boss 是否没从 Frame.From.ID 承接？）")
	}
	// 记忆服务收到的 recall 里 boss 也应是 Frame.From.ID（否则 resume 后 boss 丢失）。
	recalls := mem.RecallContexts()
	if len(recalls) == 0 {
		t.Fatal("memory 没收到 recall")
	}
	if recalls[0].Boss != "stub-boss" {
		t.Fatalf("recall 的 boss = %q, want stub-boss", recalls[0].Boss)
	}

	// trace 真的原样沿链（上面注释承诺的事，这里才是断言）：入口 call 的帧头 work 是
	// "w-native-chain"，它应当出现在链上每一帧的 work 里——
	//   reasoner → memory 的 recall 带着它 →（替身用同一个 work 回 resume）→
	//   任务把帧头 work 当作自己的 trace → 交付 deliver 与落库 store 仍带着它。
	const wantTrace = "w-native-chain"
	if got := mem.Works("recall"); len(got) != 1 || got[0] != wantTrace {
		t.Fatalf("recall 的帧头 work 应为 [%q]，实际 %q", wantTrace, got)
	}
	if got := boss.Works("deliver"); len(got) != 1 || got[0] != wantTrace {
		t.Fatalf("deliver 的帧头 work 应为 [%q]，实际 %q（trace 在最后一跳丢了？）", wantTrace, got)
	}
	// store 是 Fire（发出不等回执），落库时刻不定：只等「到没到」，不放宽「是不是同一个 trace」。
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(mem.Works("store")) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	stores := mem.Works("store")
	if len(stores) == 0 {
		t.Fatal("memory 没收到 store（trace 断言无从落脚）")
	}
	for i, w := range stores {
		if w != wantTrace {
			t.Fatalf("store 第 %d 帧的 work = %q，应等于入口 trace %q", i, w, wantTrace)
		}
	}
}
