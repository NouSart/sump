package service

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func bridgePeerCard(name string) []protocol.Provide {
	return []protocol.Provide{{Action: "echo", Tool: true, Input: "{text}", Output: "{text}"}}
}

func TestSPv2Addr(t *testing.T) {
	cases := map[string]string{
		"ws://h:1":      "ws://h:1/spv2",
		"ws://h:1/":     "ws://h:1/spv2",
		"ws://h:1/spv2": "ws://h:1/spv2",
	}
	for in, want := range cases {
		if got := spv2Addr(in); got != want {
			t.Errorf("spv2Addr(%q)=%q want %q", in, got, want)
		}
	}
}

func TestNativeMethodInbound(t *testing.T) {
	peer := New(Config{Name: "peer", Provides: bridgePeerCard("peer"), SecretFile: "-"})
	peer.HandleNative(spv2.Method{
		Name: "echo", Tool: true,
		Input:  map[string]any{"type": "object"},
		Output: map[string]any{"type": "object"},
	}, func(_ context.Context, call *spv2.Call) error {
		if call.Frame.From.ID != "caller" || call.Frame.Work != "w1" {
			t.Errorf("frame from/work = %+v", call.Frame)
		}
		var text string
		if err := call.Frame.DecodeBody(&text); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		return call.Reply("echo:" + text)
	})
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(peer.spv2Srv.Handler())
	defer ts.Close()
	c, err := spv2.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := spv2.NewCall("w1", spv2.ServiceRef("caller"), spv2.ServiceRef("peer"), "echo", "hi")
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := c.CallDecode(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	if out != "echo:hi" {
		t.Fatalf("out = %q", out)
	}
}

func TestCallOutbound(t *testing.T) {
	peer := New(Config{Name: "peer", Provides: bridgePeerCard("peer"), SecretFile: "-"})
	peer.HandleNative(spv2.Method{
		Name: "echo", Tool: true,
		Input:  map[string]any{"type": "object"},
		Output: map[string]any{"type": "object"},
	}, func(_ context.Context, call *spv2.Call) error {
		return call.Reply("from-spv2")
	})
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(peer.spv2Srv.Handler())
	defer ts.Close()

	caller := New(Config{Name: "caller", SecretFile: "-"})
	caller.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{{
		Name: "peer", Addr: "ws" + strings.TrimPrefix(ts.URL, "http") + "/spv2",
	}}})
	raw, err := caller.Call(context.Background(), "peer", "echo", "hi", "w1")
	if err != nil {
		t.Fatal(err)
	}
	var out string
	if err := protocol.DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != "from-spv2" {
		t.Fatalf("out = %q", out)
	}
}

// TestBuildSPv2Card 验证服务卡只有原生 method + 骨架方法，没有 v1 动作合成的条目。
func TestBuildSPv2Card(t *testing.T) {
	peer := New(Config{Name: "peer", Provides: bridgePeerCard("peer"), SecretFile: "-"})
	peer.HandleNative(spv2.Method{Name: "echo", Tool: true}, func(_ context.Context, _ *spv2.Call) error { return nil })
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	card := peer.spv2Srv.Card()
	names := map[string]bool{}
	for _, m := range card.Methods {
		names[m.Name] = true
	}
	for _, want := range []string{"echo", "configure", "hello", "health", "metrics"} {
		if !names[want] {
			t.Errorf("服务卡缺少 method %q: %+v", want, names)
		}
	}
}

func TestSPv2NativeHandler(t *testing.T) {
	peer := New(Config{Name: "peer", SecretFile: "-", Provides: bridgePeerCard("peer")})
	peer.HandleNative(spv2.Method{
		Name: "native_echo", Tool: true,
		Input:  map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}},
		Output: map[string]any{"type": "object"},
	}, func(_ context.Context, call *spv2.Call) error {
		var in struct {
			Text string `msgpack:"text"`
		}
		if err := call.Frame.DecodeBody(&in); err != nil {
			return call.ReplyError(spv2.CodeInvalidArgument, err.Error(), false, nil)
		}
		return call.Reply(map[string]string{"text": in.Text + "!"})
	})
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(peer.spv2Srv.Handler())
	defer ts.Close()
	c, err := spv2.Dial(context.Background(), "ws"+strings.TrimPrefix(ts.URL, "http"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := spv2.NewCall("w1", spv2.ServiceRef("caller"), spv2.ServiceRef("peer"), "native_echo", map[string]string{"text": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]string
	if err := c.CallDecode(context.Background(), f, &out); err != nil {
		t.Fatal(err)
	}
	if out["text"] != "hi!" {
		t.Fatalf("out = %+v", out)
	}
}
