package server

// /spv2 原生路径的端到端测试：真服务（最小设置中心替身）+ 真 SPv2 客户端。
// 钉住四件事：卡片带四条的 Input/Output schema 且不暴露给模型；原生 handler 能直接解 body；
// 审批 / 裁决 / 拉取 / 能力查询都有成功路径；非门户越权与坏 body 如实回错误（不合成 v1 信封）。

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/approval/engine"
	"github.com/NouSart/sump/approval/policy"
	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

var nativeTestUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// nativeTestCenter 是只做注册、名册与 list_settings 的最小设置中心替身。
type nativeTestCenter struct {
	ln  net.Listener
	srv *http.Server
}

func startNativeTestCenter(t *testing.T) *nativeTestCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	c := &nativeTestCenter{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", c.handleWS)
	c.srv = &http.Server{Handler: mux}
	go func() { _ = c.srv.Serve(ln) }()
	t.Cleanup(func() { _ = c.srv.Close() })
	return c
}

func (c *nativeTestCenter) URL() string { return "ws://" + c.ln.Addr().String() + "/ws" }

func (c *nativeTestCenter) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := nativeTestUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer ws.Close()
	cards := map[string]protocol.ServiceCard{
		"settings-center": {Name: "settings-center", Addr: c.URL(), Provides: []protocol.Provide{{Action: "list_settings"}}},
	}
	send := func(env protocol.Envelope, payload any) {
		data, err := protocol.EncodePayload(payload)
		if err != nil {
			return
		}
		resp, err := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: data})
		if err != nil {
			return
		}
		raw, err := protocol.Marshal(resp)
		if err != nil {
			return
		}
		_ = ws.WriteMessage(websocket.BinaryMessage, raw)
	}
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeRegister:
			var reg protocol.RegisterPayload
			if err := env.DecodePayload(&reg); err != nil {
				continue
			}
			cards[reg.Name] = reg.Card()
			services := make([]protocol.ServiceCard, 0, len(cards))
			for _, card := range cards {
				services = append(services, card)
			}
			send(env, protocol.RosterPayload{Services: services, Revision: 1})
		case protocol.TypeJump:
			// 设置中心替身只认 list_settings 一个动作；这里只看动作名，不引入 v1 跳转类型。
			var jp struct {
				Action string `msgpack:"action"`
			}
			if err := env.DecodePayload(&jp); err != nil {
				continue
			}
			if jp.Action == "list_settings" {
				send(env, protocol.SettingsResult{})
			}
		}
	}
}

// TestNativeSPv2Methods 验证四条原生 method 的卡片与调用路径。
func TestNativeSPv2Methods(t *testing.T) {
	t.Setenv("SUMP_SPV2", "1")
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

	conn, err := spv2.Dial(ctx, strings.TrimSuffix(s.WsURL(), "/ws")+"/spv2")
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
