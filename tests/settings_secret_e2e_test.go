package tests

// 秘密项与只读项的全链路（真进程）：面板提交的秘密只经过中心转发一次，
// 中心不落盘、不回显、事件不带值；值由 reasoner 自己持久化并在重启后带回来。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// findServiceSetting 从 list_settings 结果里取某服务的某一项。
func findServiceSetting(t *testing.T, res protocol.SettingsResult, service, key string) protocol.SettingView {
	t.Helper()
	for _, ss := range res.Services {
		if ss.Service != service {
			continue
		}
		for _, v := range ss.Settings {
			if v.Key == key {
				return v
			}
		}
	}
	t.Fatalf("结果里没有 %s.%s: %+v", service, key, res)
	return protocol.SettingView{}
}

// listSettings 以观察者身份拉一次设置。
func listSettings(t *testing.T, obs *protocol.Client, service string) protocol.SettingsResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rp := callJump(t, obs, "settings-center", "list_settings", protocol.SettingsListPayload{Service: service}, ctx)
	if !rp.OK {
		t.Fatalf("list_settings 失败: %s", rp.Error)
	}
	var res protocol.SettingsResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	return res
}

// waitSecretFlag 轮询到某秘密项的「服务侧有值」标记变为 want。
func waitSecretFlag(t *testing.T, obs *protocol.Client, service, key string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if v := findServiceSetting(t, listSettings(t, obs, service), service, key); v.Set == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待 %s.%s 的 set 标记变为 %v 超时", service, key, want)
}

// waitSetEvent 等到出现指定的 set_setting 结果事件并取出该键的那条。
func waitSetEvent(t *testing.T, rec *settingsRecorder, service, key string, timeout time.Duration) protocol.SettingResult {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		evs := append([]protocol.SettingsEventPayload(nil), rec.events...)
		rec.mu.Unlock()
		for _, e := range evs {
			if e.Cause != "set_setting" || e.Service != service {
				continue
			}
			for _, r := range e.Results {
				if r.Key == key {
					return r
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待 %s.%s 的 set_setting 结果事件超时", service, key)
	return protocol.SettingResult{}
}

// waitEventCause 等到指定 cause + 服务的 settings 事件出现。
func waitEventCause(t *testing.T, rec *settingsRecorder, cause, service string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rec.mu.Lock()
		evs := append([]protocol.SettingsEventPayload(nil), rec.events...)
		rec.mu.Unlock()
		for _, e := range evs {
			if e.Cause == cause && e.Service == service {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等待 cause=%s / service=%s 的 settings 事件超时", cause, service)
}

// TestSettingsSecretE2E：秘密项的「谁消费谁持有」边界（中心手里不留值）。
func TestSettingsSecretE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	rsBin := buildServiceNamed(t, "github.com/QAQ-awa-QAQ/sump/reasoner", "sump-reasoner")

	scPort := freePort(t)
	rsPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	reasonerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", rsPort)

	dir := t.TempDir()
	centerFile := filepath.Join(dir, "settings.json")
	secretFile := filepath.Join(dir, "reasoner-secret.json")
	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort), "-settings", centerFile)
	waitWSReady(t, centerURL, 60*time.Second)
	startProc(t, rsBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", rsPort), "-center", centerURL,
		"-secret-file", secretFile)
	waitWSReady(t, reasonerURL, 60*time.Second)

	obs, rec := registerObserver(t, centerURL)
	waitRosterHas(t, obs, "reasoner", 60*time.Second)

	// 名片：秘密项只报「还没有值」，只读项带着启动值
	res := listSettings(t, obs, "reasoner")
	key := findServiceSetting(t, res, "reasoner", "llm.key")
	if !key.Secret || key.Set || key.Value != "" || key.Default != "" {
		t.Fatalf("llm.key 声明不符（只该有 secret 标记）: %+v", key)
	}
	addr := findServiceSetting(t, res, "reasoner", "addr")
	if !addr.ReadOnly || addr.Value != fmt.Sprintf("127.0.0.1:%d", rsPort) {
		t.Fatalf("addr 只读项不符（值应为启动地址）: %+v", addr)
	}

	const secret = "sk-e2e-秘密-4c21"
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	rp := callJump(t, obs, "settings-center", "set_setting", protocol.SetSettingPayload{
		Service: "reasoner", Key: "llm.key", Value: secret,
	}, ctx)
	cancel()
	if !rp.OK {
		t.Fatalf("set_setting 被拒: %s", rp.Error)
	}
	var setRes protocol.SettingsResult
	if err := protocol.DecodeRaw(rp.Data, &setRes); err != nil {
		t.Fatal(err)
	}
	if v := findServiceSetting(t, setRes, "reasoner", "llm.key"); v.Value != "" {
		t.Fatalf("受理回执不该回显秘密值: %+v", v)
	}

	// 事件：ok=true 但不带值
	if r := waitSetEvent(t, rec, "reasoner", "llm.key", 30*time.Second); !r.OK || r.Value != "" {
		t.Fatalf("秘密项结果事件不符（ok=true 且不带值）: %+v", r)
	}

	// 值到了服务自己手里：本地文件有（中心盘上的检查放到普通项落盘之后做，
	// 那时文件才真的存在——只写秘密的话中心盘上什么都不会生成）
	waitSecretFlag(t, obs, "reasoner", "llm.key", true, 30*time.Second)
	secretRaw, err := os.ReadFile(secretFile)
	if err != nil || !strings.Contains(string(secretRaw), secret) {
		t.Fatalf("秘密没落到服务自己的文件里: %v / %s", err, secretRaw)
	}

	// 只读项：改它被明确拒绝，中心盘上也不留痕
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	rp = callJump(t, obs, "settings-center", "set_setting", protocol.SetSettingPayload{
		Service: "reasoner", Key: "addr", Value: "0.0.0.0:1",
	}, ctx)
	cancel()
	if rp.OK || !strings.Contains(rp.Error, "只读") {
		t.Fatalf("改只读项应被拒: %+v", rp)
	}

	// 普通项：中心传值真的到具体服务里并生效（真进程：受理回执 + 结果事件都 ok）
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	rp = callJump(t, obs, "settings-center", "set_setting", protocol.SetSettingPayload{
		Service: "reasoner", Key: "llm.model", Value: "model-e2e",
	}, ctx)
	cancel()
	if !rp.OK {
		t.Fatalf("set_setting(llm.model) 被拒: %s", rp.Error)
	}
	if r := waitSetEvent(t, rec, "reasoner", "llm.model", 30*time.Second); !r.OK || r.Value != "model-e2e" {
		t.Fatalf("llm.model 未在服务侧生效: %+v", r)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	rp = callJump(t, obs, "settings-center", "list_settings", protocol.SettingsListPayload{Service: "reasoner"}, ctx)
	cancel()
	var lsRes protocol.SettingsResult
	if err := protocol.DecodeRaw(rp.Data, &lsRes); err != nil {
		t.Fatal(err)
	}
	if v := findServiceSetting(t, lsRes, "reasoner", "llm.model"); v.Value != "model-e2e" || !v.Overridden {
		t.Fatalf("生效值应为覆盖后的值: %+v", v)
	}

	// 服务侧的值自己变了 → 实时回传中心（这一跳由观察者代发，验的是中心这半边的端到端；
	// 服务那半边由 service 骨架的 TestAnnounceSetting 覆盖）
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	rp = callJump(t, obs, "settings-center", "report_setting", protocol.ReportSettingPayload{
		Service: "reasoner", Key: "llm.model", Value: "model-self", Seq: 7,
	}, ctx)
	cancel()
	if !rp.OK {
		t.Fatalf("report_setting 被拒: %s", rp.Error)
	}
	v2 := findServiceSetting(t, listSettings(t, obs, "reasoner"), "reasoner", "llm.model")
	if !v2.Reported || v2.Actual != "model-self" || v2.Value != "model-e2e" || !v2.Overridden {
		t.Fatalf("回传后应「中心口径照旧 + 服务侧另记一列」: %+v", v2)
	}
	waitEventCause(t, rec, "report_setting", "reasoner", 15*time.Second)

	// 中心盘上的对照：普通项覆盖值在，秘密值 / 秘密键名 / 服务侧实际值都不在
	centerRaw, err := os.ReadFile(centerFile)
	if err != nil {
		t.Fatalf("普通项覆盖应已落盘: %v", err)
	}
	if !strings.Contains(string(centerRaw), "model-e2e") {
		t.Fatalf("普通项覆盖没落盘，测试前提不成立: %s", centerRaw)
	}
	for _, leak := range []string{secret, "llm.key", "model-self"} {
		if strings.Contains(string(centerRaw), leak) {
			t.Fatalf("中心盘上不该有 %q: %s", leak, centerRaw)
		}
	}

	// 服务重启（同一条秘密文件路径）：值由服务自己带回来，中心始终没有值可下发
	rsPort2 := freePort(t)
	reasonerURL2 := fmt.Sprintf("ws://127.0.0.1:%d/ws", rsPort2)
	startProc(t, rsBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", rsPort2), "-center", centerURL,
		"-secret-file", secretFile)
	waitWSReady(t, reasonerURL2, 60*time.Second)
	waitRosterAddr(t, obs, "reasoner", reasonerURL2, 60*time.Second)
	if v := findServiceSetting(t, listSettings(t, obs, "reasoner"), "reasoner", "llm.key"); !v.Secret || !v.Set || v.Value != "" {
		t.Fatalf("重启后应仍显示「服务侧有值」且不回显: %+v", v)
	}

	// 清空：推空值过去让服务自己清（中心依旧不留值）
	ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	rp = callJump(t, obs, "settings-center", "reset_setting", protocol.ResetSettingPayload{
		Service: "reasoner", Key: "llm.key",
	}, ctx)
	cancel()
	if !rp.OK {
		t.Fatalf("清空秘密被拒: %s", rp.Error)
	}
	waitSecretFlag(t, obs, "reasoner", "llm.key", false, 30*time.Second)
	if raw, err := os.ReadFile(secretFile); err == nil && strings.Contains(string(raw), secret) {
		t.Fatalf("清空后服务文件里还留着值: %s", raw)
	}
}
