package tests

import (
	"context"
	"io"
	"log"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
	"github.com/NouSart/sump/settings-center/server"
	"github.com/NouSart/sump/spv2"
)

// TestServiceRegistersOverNativeCenter 验证 service 骨架在 SPV2=1 时走 /spv2 注册。
func TestServiceRegistersOverNativeCenter(t *testing.T) {
	center, err := server.Start(server.Config{
		Addr: "127.0.0.1:0", SettingsPath: filepath.Join(t.TempDir(), "settings.json"),
	}, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = center.Shutdown() }()

	applied := make(chan string, 1)
	svc := service.New(service.Config{
		Name: "native-peer", Listen: "127.0.0.1:0", Center: "ws://" + center.Addr() + "/ws",
		SPV2: true, SecretFile: "-", Logger: log.New(io.Discard, "", 0),
	})
	svc.HandleSetting("msg", "default", func(v string) error {
		select {
		case applied <- v:
		default:
		}
		return nil
	})
	defer svc.Shutdown()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, card := range svc.Roster() {
		if card.Name == "native-peer" {
			found = true
		}
	}
	if !found {
		t.Fatalf("原生注册后名册里没有自己: %+v", svc.Roster())
	}

	// 中心通过原生 /spv2 下发 configure，服务原生 handler 应用设置。
	cc, err := spv2.Dial(ctx, "ws://"+center.Addr()+"/spv2")
	if err != nil {
		t.Fatal(err)
	}
	defer cc.Close()
	set, err := spv2.NewCall("w-set", spv2.ServiceRef("admin"), spv2.ServiceRef("settings-center"), "set_setting",
		protocol.SetSettingPayload{Service: "native-peer", Key: "msg", Value: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := cc.Call(ctx, set); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(3 * time.Second)
	for {
		select {
		case got := <-applied:
			if got == "hello" {
				return
			}
			// 注册时的默认值下发先到；继续等这次 set_setting 的 hello。
		case <-deadline:
			t.Fatal("没有收到原生 configure")
		}
	}
}
