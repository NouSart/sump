package service

// 这一段把 src/service 骨架到 SPv2.1 的映射事实钉成机器可检查的断言（不是注释）：
//
//   - 传出去的 trace 变成帧头的 work：work 是 SPv2.1 唯一的工作名词，骨架没有第二个 id 可放。
//   - 「结果归属」逐跳自定，靠的是帧头 from（本服务的名字），不是调用方传的参数。
//   - Call / Fire 的 boss 参数**不上线**：SPv2.1 帧头没有 boss 字段，
//     callSPv2 也从不看它。生产代码在载荷里另带 boss，所以这不是丢数据，
//     而是遗留参数——留着它需要改所有服务，那属于协议讨论，不在本轮范围。
//     这里断言的是「线上确实没有它」，将来要清掉时也有测试可对照。
//
// 对照的旧用例 TestCallOutbound 只断言了返回字符串，对端 handler 甚至没看帧头，
// 所以 work/from 在这条链路上丢了也不会变红——下面两条把它补齐。

import (
	"bytes"
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

// bridgePeer 起一个只暴露 echo 的原生服务端，把收到的每一帧投到返回的 channel。
func bridgePeer(t *testing.T, name string, reply func(*spv2.Call)) (wsURL string, frames chan spv2.Frame) {
	t.Helper()
	peer := New(Config{Name: name, Provides: bridgePeerCard(name), SecretFile: "-"})
	in := make(chan spv2.Frame, 8)
	peer.HandleNative(spv2.Method{
		Name: "echo", Tool: true,
		Input:  map[string]any{"type": "object"},
		Output: map[string]any{"type": "object"},
	}, func(_ context.Context, call *spv2.Call) error {
		in <- call.Frame
		if reply != nil {
			reply(call)
			return nil
		}
		var text string
		if err := call.Frame.DecodeBody(&text); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		return call.Reply("echo:" + text)
	})
	if err := peer.buildSPv2(); err != nil {
		t.Fatalf("%s buildSPv2: %v", name, err)
	}
	ts := httptest.NewServer(peer.spv2Srv.Handler())
	t.Cleanup(ts.Close)
	return "ws" + strings.TrimPrefix(ts.URL, "http"), in
}

// recvFrame 在超时内取一帧；取不到就说明这一跳根本没送达。
func recvFrame(t *testing.T, frames chan spv2.Frame) spv2.Frame {
	t.Helper()
	select {
	case f := <-frames:
		return f
	case <-time.After(5 * time.Second):
		t.Fatal("对端没有收到帧")
		return spv2.Frame{}
	}
}

// TestBridgeTraceArrivesAsWork：一跳之内 trace → work，且 from 是自定的本服务身份。
func TestBridgeTraceArrivesAsWork(t *testing.T) {
	peerURL, frames := bridgePeer(t, "peer", nil)

	caller := New(Config{Name: "caller", SecretFile: "-"})
	caller.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{
		{Name: "peer", Addr: peerURL + "/spv2"},
	}})

	raw, err := caller.Call(context.Background(), "peer", "echo", "hi", "trace-77", "boss")
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	var out string
	if err := protocol.DecodeRaw(raw, &out); err != nil {
		t.Fatalf("DecodeRaw: %v", err)
	}
	if out != "echo:hi" {
		t.Fatalf("out = %q, want echo:hi", out)
	}

	f := recvFrame(t, frames)
	if f.Type != spv2.TypeCall || f.Method != "echo" {
		t.Fatalf("对端收到的帧头不符: %+v", f)
	}
	if f.Work != "trace-77" {
		t.Fatalf("work = %q, want 调用方传的 trace %q（work 是唯一的工作名词）", f.Work, "trace-77")
	}
	// 归属逐跳自定：from 必须是发起这一跳的服务自己，不是任何参数。
	if f.From != spv2.ServiceRef("caller") {
		t.Fatalf("from = %+v, want service/caller", f.From)
	}
	if f.To != spv2.ServiceRef("peer") {
		t.Fatalf("to = %+v, want service/peer", f.To)
	}

	// boss 是遗留参数：整帧上线后，键名与值里都不该出现它。
	wire, err := spv2.Marshal(f)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if bytes.Contains(wire, []byte("boss")) {
		t.Fatalf("boss 竟然上线了: 帧头 %+v", f)
	}
}

// TestBridgeEmptyTraceGetsGeneratedWork：trace 为空时骨架必须自己造一个 work id，
// 否则帧头校验（work 必填）会直接把这一跳打死。
func TestBridgeEmptyTraceGetsGeneratedWork(t *testing.T) {
	peerURL, frames := bridgePeer(t, "peer", nil)

	caller := New(Config{Name: "caller", SecretFile: "-"})
	caller.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{
		{Name: "peer", Addr: peerURL + "/spv2"},
	}})

	if _, err := caller.Call(context.Background(), "peer", "echo", "hi", "", "boss"); err != nil {
		t.Fatalf("空 trace 的这一跳失败: %v", err)
	}
	f := recvFrame(t, frames)
	if f.Work == "" {
		t.Fatal("work 为空：call 帧没有 work 就非法")
	}
	if f.From != spv2.ServiceRef("caller") {
		t.Fatalf("from = %+v, want service/caller", f.From)
	}
}

// TestBridgeWorkSurvivesMultiHop：两跳链路必须共用同一个 work id。
// 中间服务把自己收到帧的 work 原样作为下一跳的 trace 传出去——这正是各服务在链路上的做法。
func TestBridgeWorkSurvivesMultiHop(t *testing.T) {
	sinkURL, sinkFrames := bridgePeer(t, "sink", nil)

	mid := New(Config{Name: "mid", SecretFile: "-"})
	mid.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{
		{Name: "sink", Addr: sinkURL + "/spv2"},
	}})
	forwarded := make(chan string, 1)
	mid.HandleNative(spv2.Method{Name: "forward", Tool: true},
		func(ctx context.Context, call *spv2.Call) error {
			// 下一跳的 trace 用**收到的 work**：work 就是链路上唯一的工作名词。
			_, err := mid.Call(ctx, "sink", "echo", "hop2", call.Frame.Work, "boss")
			if err != nil {
				return call.ReplyError(spv2.CodeInternal, err.Error(), false, nil)
			}
			forwarded <- call.Frame.Work
			return call.Reply("forwarded")
		})
	if err := mid.buildSPv2(); err != nil {
		t.Fatalf("mid buildSPv2: %v", err)
	}
	midTS := httptest.NewServer(mid.spv2Srv.Handler())
	defer midTS.Close()

	caller := New(Config{Name: "caller", SecretFile: "-"})
	caller.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{
		{Name: "mid", Addr: "ws" + strings.TrimPrefix(midTS.URL, "http") + "/spv2"},
	}})
	if _, err := caller.Call(context.Background(), "mid", "forward", "go", "trace-chain", "boss"); err != nil {
		t.Fatalf("第一跳失败: %v", err)
	}

	select {
	case w := <-forwarded:
		if w != "trace-chain" {
			t.Fatalf("中间服务收到的 work = %q, want %q", w, "trace-chain")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("中间服务没有转发")
	}

	hop2 := recvFrame(t, sinkFrames)
	if hop2.Work != "trace-chain" {
		t.Fatalf("第二跳的 work = %q, want 全链路同一个 %q", hop2.Work, "trace-chain")
	}
	// 逐跳自定：第二跳的 from 必须是 mid，而不是最初的 caller。
	if hop2.From != spv2.ServiceRef("mid") {
		t.Fatalf("第二跳 from = %+v, want service/mid", hop2.From)
	}
	if hop2.To != spv2.ServiceRef("sink") {
		t.Fatalf("第二跳 to = %+v, want service/sink", hop2.To)
	}
}
