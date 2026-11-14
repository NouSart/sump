package server

// /spv2 原生路径的端到端测试：真服务（最小设置中心替身）+ 真 SPv2 客户端。
// 钉住四件事：卡片带四条的 Input/Output schema 且不暴露给模型；原生 handler 能直接解 body；
// 审批 / 裁决 / 拉取 / 能力查询都有成功路径；非门户越权与坏 body 如实回错误（不合成 v1 信封）。

import (
	"context"
	"errors"
	"io"
	"log"
	"net/http/httptest"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// nativeTestCenter 是只做注册、名册与 list_settings 的最小设置中心替身（全走 /spv2）。
type nativeTestCenter struct {
	srv  *spv2.Server
	http *httptest.Server

	mu    sync.Mutex
	cards map[string]protocol.ServiceCard
}

func startNativeTestCenter(t *testing.T) *nativeTestCenter {
	t.Helper()
	srv, err := spv2.NewServer(spv2.Card{
		Protocol: spv2.ProtocolVersion, Name: "settings-center", Actor: spv2.ServiceRef("settings-center"),
		Version: "stub/1", Methods: []spv2.Method{
			{Name: "register", Shape: spv2.ShapeCall},
			{Name: "list_settings", Shape: spv2.ShapeCall},
			{Name: "heartbeat", Shape: spv2.ShapeEvent},
		},
		Limits: spv2.Limits{MaxBody: 1 << 20},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := &nativeTestCenter{srv: srv, cards: map[string]protocol.ServiceCard{}}
	srv.Handle("register", c.handleRegister)
	srv.Handle("list_settings", func(_ context.Context, call *spv2.Call) error {
		return call.Reply(protocol.SettingsResult{})
	})
	// 心跳是事件，没有回执可发；收下即可。
	srv.Handle("heartbeat", func(context.Context, *spv2.Call) error { return nil })
	c.http = httptest.NewServer(srv.Handler())
	t.Cleanup(c.http.Close)
	c.cards["settings-center"] = protocol.ServiceCard{
		Name: "settings-center", Addr: c.URL(), Description: "设置中心替身",
		Provides: []protocol.Provide{{Action: "list_settings"}, {Action: "roster"}},
	}
	return c
}

func (c *nativeTestCenter) URL() string {
	return "ws" + strings.TrimPrefix(c.http.URL, "http") + "/spv2"
}

func (c *nativeTestCenter) handleRegister(_ context.Context, call *spv2.Call) error {
	var reg protocol.RegisterPayload
	if err := call.Frame.DecodeBody(&reg); err != nil {
		return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
	}
	c.mu.Lock()
	c.cards[reg.Name] = reg.Card()
	services := make([]protocol.ServiceCard, 0, len(c.cards))
	for _, card := range c.cards {
		services = append(services, card)
	}
	c.mu.Unlock()
	return call.Reply(protocol.RosterPayload{Services: services, Revision: 1})
}

// TestNativeSPv2Methods 验证四条原生 method 的卡片与调用路径。
func TestNativeSPv2Methods(t *testing.T) {
	center := startNativeTestCenter(t)
	exe := filepath.Join(t.TempDir(), "pwsh.exe")
	e, err := engine.New(echoParser{path: exe}, policy.DefaultJSON)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(Config{
		Name: "approval", Listen: "127.0.0.1:0", Center: center.URL(),
		Engine: e, PolicyJSON: policy.DefaultJSON, Logger: log.New(io.Discard, "", 0),
		Portal: "tester", ReviewTTL: 2 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer s.Shutdown()

	conn, err := spv2.Dial(ctx, s.WsURL())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	card, err := conn.Hello(ctx, spv2.ServiceRef("tester"), spv2.ServiceRef("approval"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{protocol.ApprovalAction, protocol.ApprovalDecideAction, protocol.ApprovalListAction, "capabilities"} {
		m, ok := card.Method(name)
		if !ok {
			t.Fatalf("卡片缺少原生 method %s", name)
		}
		if len(m.Input) == 0 || len(m.Output) == 0 {
			t.Fatalf("原生 method %s 缺少 Input/Output schema", name)
		}
		if m.Tool {
			t.Fatalf("原生 method %s 不应暴露为模型工具", name)
		}
	}

	req := protocol.CommandApprovalRequest{Version: 1, ItemID: "item-native", Plan: protocol.ExecutionPlan{
		Version: 1, Command: "Get-Date", WorkDir: filepath.Dir(exe), ShellPath: exe, Args: protocol.PowerShellArgs(),
		Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile,
		TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
	call, err := spv2.NewCall("w-native", spv2.ServiceRef("tester"), spv2.ServiceRef("approval"), protocol.ApprovalAction, req)
	if err != nil {
		t.Fatal(err)
	}
	var res protocol.CommandApprovalResult
	if err := conn.CallDecode(ctx, call, &res); err != nil {
		t.Fatal(err)
	}
	if res.Decision != protocol.DecisionAllow {
		t.Fatalf("原生 approval_request 应直接放行白名单命令: %+v", res)
	}

	// 第二层：review 命令挂起后，门户通过原生 approval_decide 裁决，调用方拿回 allow。
	s.portalReady = func() bool { return true }
	reviewReq := protocol.CommandApprovalRequest{Version: 1, ItemID: "item-native-review", Plan: protocol.ExecutionPlan{
		Version: 1, Command: "Get-Process", WorkDir: filepath.Dir(exe), ShellPath: exe, Args: protocol.PowerShellArgs(),
		Dialect: "powershell", Platform: runtime.GOOS, Profile: protocol.PowerShellProfile,
		TimeoutMS: 5000, MaxOutputBytes: 4096,
	}}
	reviewCall, err := spv2.NewCall("w-native", spv2.ServiceRef("tester"), spv2.ServiceRef("approval"), protocol.ApprovalAction, reviewReq)
	if err != nil {
		t.Fatal(err)
	}
	reviewed := make(chan protocol.CommandApprovalResult, 1)
	go func() {
		var got protocol.CommandApprovalResult
		if err := conn.CallDecode(ctx, reviewCall, &got); err == nil {
			reviewed <- got
		}
	}()
	waitPending(t, s, reviewReq.ItemID)
	decideCall, err := spv2.NewCall("w-native", spv2.ServiceRef("tester"), spv2.ServiceRef("approval"), protocol.ApprovalDecideAction,
		protocol.ApprovalDecision{ItemID: reviewReq.ItemID, Decision: protocol.DecisionAllow, Note: "原生裁决"})
	if err != nil {
		t.Fatal(err)
	}
	var decided struct {
		Status   string `msgpack:"status"`
		ItemID   string `msgpack:"item_id"`
		Decision string `msgpack:"decision"`
	}
	if err := conn.CallDecode(ctx, decideCall, &decided); err != nil {
		t.Fatalf("原生 approval_decide 失败: %v", err)
	}
	if decided.Status != "accepted" || decided.ItemID != reviewReq.ItemID || decided.Decision != protocol.DecisionAllow {
		t.Fatalf("原生裁决结果不对: %+v", decided)
	}
	select {
	case got := <-reviewed:
		if got.Decision != protocol.DecisionAllow || got.ReasonCode != protocol.ReasonUserApproved {
			t.Fatalf("原生 approve 调用方未拿到用户批准: %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("原生裁决后调用方没被唤醒")
	}

	// §12.3：审批域最小指标必须真的落到服务的 SPv2 注册表（否则 /metrics 里看不到）。
	var decisionOK, waitOK bool
	for _, m := range s.Metrics().Snapshot() {
		if m.Name == "sump_approval_decisions_total" && m.Labels["decision"] == protocol.DecisionAllow && m.Value >= 1 {
			decisionOK = true
		}
		if m.Name == "sump_approval_wait_seconds" && m.Count >= 1 {
			waitOK = true
		}
	}
	if !decisionOK || !waitOK {
		t.Fatalf("审批指标缺失: decisionOK=%v waitOK=%v snapshot=%+v", decisionOK, waitOK, s.Metrics().Snapshot())
	}

	listCall, err := spv2.NewCall("w-native", spv2.ServiceRef("tester"), spv2.ServiceRef("approval"), protocol.ApprovalListAction, nil)
	if err != nil {
		t.Fatal(err)
	}
	var list protocol.PendingApprovalList
	if err := conn.CallDecode(ctx, listCall, &list); err != nil {
		t.Fatalf("门户拉取待审项失败: %v", err)
	}
	if len(list.Items) != 0 {
		t.Fatalf("空队列应返回空列表: %+v", list)
	}

	// 非门户不能裁决 / 拉取（原生路径同样要过权限闸门）。
	intruder := spv2.ServiceRef("intruder")
	badList, err := spv2.NewCall("w-native", intruder, spv2.ServiceRef("approval"), protocol.ApprovalListAction, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CallDecode(ctx, badList, &list); !isNativeCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非门户拉取应回 invalid_argument，实际: %v", err)
	}
	badDecide, err := spv2.NewCall("w-native", intruder, spv2.ServiceRef("approval"), protocol.ApprovalDecideAction,
		protocol.ApprovalDecision{ItemID: "x", Decision: protocol.DecisionAllow})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CallDecode(ctx, badDecide, &map[string]any{}); !isNativeCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("非门户裁决应回 invalid_argument，实际: %v", err)
	}

	// 原生入口对坏 body 要回错误，不能 panic 或静默成功。
	badBody, err := spv2.NewCall("w-native", spv2.ServiceRef("tester"), spv2.ServiceRef("approval"), protocol.ApprovalAction, []byte{0xff})
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CallDecode(ctx, badBody, &res); !isNativeCode(err, spv2.CodeInvalidArgument) {
		t.Fatalf("坏 body 应回 invalid_argument，实际: %v", err)
	}

	var caps struct {
		SchemaVersion int      `msgpack:"schema_version"`
		Dialects      []string `msgpack:"dialects"`
		ReviewPortal  string   `msgpack:"review_portal"`
		ReviewEnabled bool     `msgpack:"review_enabled"`
	}
	capsCall, err := spv2.NewCall("w-native", spv2.ServiceRef("tester"), spv2.ServiceRef("approval"), "capabilities", nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.CallDecode(ctx, capsCall, &caps); err != nil {
		t.Fatalf("原生 capabilities 失败: %v", err)
	}
	if caps.SchemaVersion != 1 || len(caps.Dialects) != 2 || !caps.ReviewEnabled || caps.ReviewPortal != "tester" {
		t.Fatalf("原生 capabilities 内容不对: %+v", caps)
	}
}

// isNativeCode 判断错误是不是指定错误码的 SPv2 错误。
func isNativeCode(err error, code string) bool {
	var se *spv2.Error
	return errors.As(err, &se) && se.Code == code
}
