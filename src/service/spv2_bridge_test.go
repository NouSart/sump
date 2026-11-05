package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/spv2"
)

func bridgePeerCard(name string) []protocol.Provide {
	return []protocol.Provide{{Action: "echo", Tool: true, Input: "{text}", Output: "{text}"}}
}

func TestSPv2Addr(t *testing.T) {
	cases := map[string]string{
		"ws://h:1/ws":   "ws://h:1/spv2",
		"ws://h:1":      "ws://h:1/spv2",
		"ws://h:1/spv2": "ws://h:1/spv2",
	}
	for in, want := range cases {
		if got := spv2Addr(in); got != want {
			t.Errorf("spv2Addr(%q)=%q want %q", in, got, want)
		}
	}
}

func TestSPv2BridgeInbound(t *testing.T) {
	peer := New(Config{Name: "peer", SPV2: true, Provides: bridgePeerCard("peer"), SecretFile: "-"})
	peer.Handle("echo", func(_ context.Context, env protocol.Envelope, in protocol.JumpPayload) (any, error) {
		if env.Boss != "caller" || env.Trace != "w1" {
			t.Errorf("synthesized env = %+v", env)
		}
		var text string
		if err := protocol.DecodeRaw(in.Input, &text); err != nil {
			return nil, err
		}
		return "echo:" + text, nil
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

func TestSPv2BridgeOutboundAndFallback(t *testing.T) {
	// 目标同时有 /ws(v1) 与 /spv2(SPv2)；SPv2 优先。
	peer := New(Config{Name: "peer", SPV2: true, Provides: bridgePeerCard("peer"), SecretFile: "-"})
	peer.Handle("echo", func(_ context.Context, _ protocol.Envelope, _ protocol.JumpPayload) (any, error) {
		return "from-spv2", nil
	})
	if err := peer.buildSPv2(); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.Handle("/spv2", peer.spv2Srv.Handler())
	mux.HandleFunc("/ws", peer.handleWS)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	caller := New(Config{Name: "caller", SPV2: true, SecretFile: "-"})
	caller.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{{
		Name: "peer", Addr: "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws",
	}}})
	raw, err := caller.Call(context.Background(), "peer", "echo", "hi", "w1", "boss")
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

	// 只有 v1 的目标：/spv2 不可达，回退 v1。
	v1peer := New(Config{Name: "v1peer", SecretFile: "-"})
	v1peer.Handle("read", func(_ context.Context, _ protocol.Envelope, _ protocol.JumpPayload) (any, error) {
		return "from-v1", nil
	})
	v1mux := http.NewServeMux()
	v1mux.HandleFunc("/ws", v1peer.handleWS)
	v1ts := httptest.NewServer(v1mux)
	defer v1ts.Close()
	caller.setRoster(protocol.RosterPayload{Services: []protocol.ServiceCard{{
		Name: "v1peer", Addr: "ws" + strings.TrimPrefix(v1ts.URL, "http") + "/ws",
	}}})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	raw, err = caller.Call(ctx, "v1peer", "read", "hi", "w2", "boss")
	if err != nil {
		t.Fatal(err)
	}
	if err := protocol.DecodeRaw(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out != "from-v1" {
		t.Fatalf("fallback out = %q", out)
	}
}
