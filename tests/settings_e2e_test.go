package tests

// M7 总测试：设置热链（真进程）——受理回执不等待送达、下发结果以 settings 事件回传、
// 行为即时生效、进程重启自动重注册并全量对齐、重置回落预设。

import (
	"context"
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// settingsRecorder 收集观察者连接上的 settings 事件。
type settingsRecorder struct {
	mu     sync.Mutex
	events []protocol.SettingsEventPayload
}

func (r *settingsRecorder) onEvent(env protocol.Envelope) {
	if env.Type != protocol.TypeSettings {
		return
	}
	var p protocol.SettingsEventPayload
	if err := env.DecodePayload(&p); err != nil {
		return
	}
	r.mu.Lock()
	r.events = append(r.events, p)
	r.mu.Unlock()
}

// wait 等待至少 n 条事件并返回快照。
func (r *settingsRecorder) wait(t *testing.T, n int, timeout time.Duration) []protocol.SettingsEventPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		r.mu.Lock()
		got := append([]protocol.SettingsEventPayload(nil), r.events...)
		r.mu.Unlock()
		if len(got) >= n {
			return got
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待 settings 事件超时（期望 %d 条）", n)
	return nil
}

// waitRosterAddr 等待名册里某服务的名片地址变为 addr（服务重启换端口后用它判定“已重新注册”）。
func waitRosterAddr(t *testing.T, obs *protocol.Client, name, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var r protocol.RosterPayload
		rp := callJump(t, obs, "settings-center", "roster", nil, ctx)
		cancel()
		if rp.OK && protocol.DecodeRaw(rp.Data, &r) == nil {
			for _, s := range r.Services {
				if s.Name == name && s.Addr == addr {
					return
				}
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待名册中 %s 的地址变为 %s 超时", name, addr)
}

// saveImage 向 images 发一次 save，返回响应 payload。
func saveImage(t *testing.T, imagesURL string, data string) protocol.ResponsePayload {
	t.Helper()
	c, err := protocol.Dial(imagesURL, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return callJump(t, c, "images", "save", protocol.ImageSavePayload{Data: data, Mime: "image/png"}, ctx)
}

// TestSettingsHotPathE2E：images.max_mb 的设置热链。
func TestSettingsHotPathE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	imgBin := buildServiceNamed(t, "github.com/QAQ-awa-QAQ/sump/images", "sump-images")

	scPort := freePort(t)
	imgPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	imagesURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", imgPort)

	dir := t.TempDir()
	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort), "-settings", filepath.Join(dir, "settings.json"))
	waitWSReady(t, centerURL, 60*time.Second)
	imgCmd := startProc(t, imgBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", imgPort), "-center", centerURL,
		"-db", filepath.Join(dir, "images.db"), "-dir", filepath.Join(dir, "images"))
	waitWSReady(t, imagesURL, 60*time.Second)

	// 观察者：注册进中心，收集 settings 广播（地址不会被拨号：无声明设置项）。
	rec := &settingsRecorder{}
	obs, err := protocol.Dial(centerURL, rec.onEvent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = obs.Close() })
	obsReg, err := protocol.NewEnvelope(protocol.TypeRegister, "e2e-settings-observer", "settings-center", "", protocol.RegisterPayload{
		Name: "e2e-settings-observer", Addr: "ws://127.0.0.1:1/ws", Description: "设置热链观察者",
	})
	if err != nil {
		t.Fatal(err)
	}
	obsCtx, obsCancel := context.WithTimeout(context.Background(), 30*time.Second)
	if _, err := obs.Call(obsCtx, obsReg); err != nil {
		t.Fatal(err)
	}
	obsCancel()
	waitRosterHas(t, obs, "images", 60*time.Second)

	// 1) set max_mb=1：受理回执立即返回（含新值视图）——不等待服务送达。
	setCtx, setCancel := context.WithTimeout(context.Background(), 30*time.Second)
	rp := callJump(t, obs, "settings-center", "set_setting", protocol.SetSettingPayload{Service: "images", Key: "max_mb", Value: "1"}, setCtx)
	setCancel()
	if !rp.OK {
		t.Fatalf("set_setting 未被受理: %s", rp.Error)
	}
	var res protocol.SettingsResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if len(res.Services) != 1 || len(res.Services[0].Settings) != 1 || res.Services[0].Settings[0].Value != "1" || !res.Services[0].Settings[0].Overridden {
		t.Fatalf("set_setting 回执视图不符: %+v", res)
	}

	// 2) 下发结果以 settings 事件回传（cause=set_setting，applied）。
	evs := rec.wait(t, 1, 30*time.Second)
	ev := evs[0]
	if ev.Cause != "set_setting" || ev.Service != "images" || len(ev.Results) != 1 || !ev.Results[0].OK || ev.Results[0].Value != "1" {
		t.Fatalf("set_setting 结果事件不符: %+v", ev)
	}

	// 3) 行为即时生效：2MiB 保存被拒（错误含新上限 1048576 字节）。
	big := base64.StdEncoding.EncodeToString(make([]byte, 2<<20))
	if r := saveImage(t, imagesURL, big); r.OK || !strings.Contains(r.Error, "1048576") {
		t.Fatalf("超限保存应被拒（含新上限）: %+v", r)
	}

	// 4) 进程重启（新端口）：自动重注册 → 中心全量下发 → 覆盖值仍生效（事件 cause=register）。
	_ = imgCmd.Process.Kill()
	_, _ = imgCmd.Process.Wait()
	imgPort2 := freePort(t)
	imagesURL2 := fmt.Sprintf("ws://127.0.0.1:%d/ws", imgPort2)
	startProc(t, imgBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", imgPort2), "-center", centerURL,
		"-db", filepath.Join(dir, "images.db"), "-dir", filepath.Join(dir, "images"))
	waitWSReady(t, imagesURL2, 60*time.Second)
	waitRosterAddr(t, obs, "images", imagesURL2, 60*time.Second)

	evs = rec.wait(t, 2, 60*time.Second)
	regEv := evs[1]
	if regEv.Cause != "register" || regEv.Service != "images" || len(regEv.Results) != 1 || !regEv.Results[0].OK || regEv.Results[0].Value != "1" {
		t.Fatalf("重启后全量对齐事件不符: %+v", regEv)
	}
	if r := saveImage(t, imagesURL2, big); r.OK || !strings.Contains(r.Error, "1048576") {
		t.Fatalf("重启后覆盖值应仍生效: %+v", r)
	}

	// 5) reset：回落预设（32MB）→ 事件 ok，2MiB 可保存。
	resetCtx, resetCancel := context.WithTimeout(context.Background(), 30*time.Second)
	rp = callJump(t, obs, "settings-center", "reset_setting", protocol.ResetSettingPayload{Service: "images", Key: "max_mb"}, resetCtx)
	resetCancel()
	if !rp.OK {
		t.Fatalf("reset_setting 未被受理: %s", rp.Error)
	}
	evs = rec.wait(t, 3, 30*time.Second)
	resetEv := evs[2]
	if resetEv.Cause != "reset_setting" || len(resetEv.Results) != 1 || !resetEv.Results[0].OK || resetEv.Results[0].Value != "32" {
		t.Fatalf("重置事件不符: %+v", resetEv)
	}
	if r := saveImage(t, imagesURL2, big); !r.OK {
		t.Fatalf("重置后 2MiB 应可保存: %s", r.Error)
	}
}
