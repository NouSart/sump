package tests

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
	"github.com/NouSart/sump/spv2"
)

// TestSettingsCenterNative 验证设置中心的 /spv2 控制面：register / roster / report。
func TestSettingsCenterNative(t *testing.T) {
	s, err := server.Start(server.Config{
		Addr: "127.0.0.1:0", SettingsPath: filepath.Join(t.TempDir(), "settings.json"),
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Shutdown() }()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, "ws://"+s.Addr()+"/spv2")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()

	reg, err := spv2.NewCall("w-reg", spv2.ServiceRef("peer"), spv2.ServiceRef("settings-center"), "register",
		protocol.RegisterPayload{Name: "peer", Addr: "ws://127.0.0.1:1/ws", Description: "native"})
	if err != nil {
		t.Fatal(err)
	}
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, reg, &roster); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, card := range roster.Services {
		if card.Name == "peer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("注册后名册里没有 peer: %+v", roster)
	}

	rosterCall, err := spv2.NewCall("w-roster", spv2.ServiceRef("peer"), spv2.ServiceRef("settings-center"), "roster", nil)
	if err != nil {
		t.Fatal(err)
	}
	var snap protocol.RosterPayload
	if err := c.CallDecode(ctx, rosterCall, &snap); err != nil {
		t.Fatal(err)
	}
	if snap.Revision < roster.Revision {
		t.Fatalf("revision 回退: %d < %d", snap.Revision, roster.Revision)
	}

	report, err := spv2.NewCall("w-report", spv2.ServiceRef("peer"), spv2.ServiceRef("settings-center"), "report",
		protocol.ReportPayload{Kind: "test", Message: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	var ack map[string]any
	if err := c.CallDecode(ctx, report, &ack); err != nil {
		t.Fatal(err)
	}
	if ack["status"] != "ok" {
		t.Fatalf("report ack = %+v", ack)
	}
}
