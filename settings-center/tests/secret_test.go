package tests

// 秘密项与只读项的保管边界（D 方案：谁消费谁持有）：
// 秘密值只在「面板 → 中心 → 服务」这一跳里出现，中心不落盘、不回显、事件里也不带；
// 只读项（启动参数）照样进面板，但改动与重置都被明确拒绝。

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/settings-center/server"
)

// waitConfigure 等待某个键被下发为指定值（超时失败）。
func waitConfigure(t *testing.T, svc *stubService, key, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		svc.mu.Lock()
		got := append([]protocol.ConfigurePayload(nil), svc.got...)
		svc.mu.Unlock()
		for _, p := range got {
			if p.Key == key && p.Value == want {
				return
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("等待 %s = %q 下发超时", key, want)
}

// waitEventCause 等待一条指定 cause 的 settings 事件，返回当时收到的全部事件。
func waitEventCause(t *testing.T, ev *eventRecorder, cause string, timeout time.Duration) []protocol.SettingsEventPayload {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		ev.mu.Lock()
		got := append([]protocol.SettingsEventPayload(nil), ev.events...)
		ev.mu.Unlock()
		for _, e := range got {
			if e.Cause == cause {
				return got
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("等待 cause=%s 的 settings 事件超时", cause)
	return nil
}

// configureForKey 取到目前为止某个键收到的全部下发（快照）。
func configureForKey(svc *stubService, key string) []protocol.ConfigurePayload {
	svc.mu.Lock()
	defer svc.mu.Unlock()
	var out []protocol.ConfigurePayload
	for _, p := range svc.got {
		if p.Key == key {
			out = append(out, p)
		}
	}
	return out
}

// waitSecretSet 轮询 list_settings 直到某秘密项的「服务侧有值」标记变为 want。
func waitSecretSet(t *testing.T, wsURL, service, key string, want bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last bool
	for time.Now().Before(deadline) {
		res, msg := callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: service})
		if msg != "" {
			t.Fatalf("list_settings 失败: %s", msg)
		}
		last = findSetting(t, res, service, key).Set
		if last == want {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("等待 %s.%s 的 set 标记变为 %v 超时（仍是 %v）", service, key, want, last)
}

// TestSecretAndReadOnly 秘密项不回显不落盘、只读项只读。
func TestSecretAndReadOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0", SettingsPath: path}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	svc := startStubService(t)
	_, events := registerService(t, wsURL, "svc-s", svc.URL(), []protocol.Setting{
		{Key: "api.base", Default: "https://api.example.com"},
		{Key: "api.key", Secret: true},
		{Key: "api.addr", Default: "127.0.0.1:9101", ReadOnly: true},
	})

	const secret = "sk-秘密-9f3a71bc"
	// 注册对齐那一轮跑完（register 事件 = 下发轮结束）：秘密项与只读项都不该被推
	waitEventCause(t, events, "register", 3*time.Second)
	if got := configureForKey(svc, "api.key"); len(got) != 0 {
		t.Fatalf("注册对齐不该推秘密项（推空值会把服务自己那份抹掉）: %+v", got)
	}
	if got := configureForKey(svc, "api.addr"); len(got) != 0 {
		t.Fatalf("注册对齐不该推只读项（服务只会拒一次，纯噪声）: %+v", got)
	}
	if got := configureForKey(svc, "api.base"); len(got) != 1 {
		t.Fatalf("普通项应在注册时对齐一次: %+v", got)
	}

	res, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{
		Service: "svc-s", Key: "api.key", Value: secret,
	})
	if msg != "" {
		t.Fatalf("set_setting 秘密项失败: %s", msg)
	}

	// 回复里只有「有秘密项」这一件事，值本身不回显
	v := findSetting(t, res, "svc-s", "api.key")
	if !v.Secret || v.Value != "" || v.Default != "" || v.Overridden {
		t.Fatalf("秘密项视图不符（值不该回显）: %+v", v)
	}

	// 值原样到达服务（这一跳是它的唯一旅程）
	waitConfigure(t, svc, "api.key", secret, 3*time.Second)

	// 另写一个普通项做对照：证明落盘确实发生了，而不是「文件压根没写」
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{
		Service: "svc-s", Key: "api.base", Value: "https://mirror.example.com",
	}); msg != "" {
		t.Fatalf("set_setting 普通项失败: %s", msg)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("mirror.example.com")) {
		t.Fatalf("普通覆盖值没落盘，测试前提不成立: %s", raw)
	}
	if bytes.Contains(raw, []byte(secret)) {
		t.Fatalf("秘密值落到了中心的盘上: %s", raw)
	}
	if bytes.Contains(raw, []byte("api.key")) {
		t.Fatalf("秘密项的键名也不该留在中心的盘上: %s", raw)
	}

	// 秘密项「服务侧有值」的标记（下发成功后置位）
	waitSecretSet(t, wsURL, "svc-s", "api.key", true, 3*time.Second)

	// 重连再注册：秘密已设置，仍然不该被推——中心手里没有值，推过去就是抹掉
	before := len(configureForKey(svc, "api.key"))
	_, events2 := registerService(t, wsURL, "svc-s", svc.URL(), []protocol.Setting{
		{Key: "api.base", Default: "https://api.example.com"},
		{Key: "api.key", Secret: true, Set: true},
		{Key: "api.addr", Default: "127.0.0.1:9101", ReadOnly: true},
	})
	waitEventCause(t, events2, "register", 3*time.Second)
	if after := len(configureForKey(svc, "api.key")); after != before {
		t.Fatalf("重连对齐推了秘密项（会把服务那份抹掉）: %d → %d 条", before, after)
	}

	// 事件与查询结果里都搜不到秘密值
	evs := waitEventCause(t, events, "set_setting", 3*time.Second)
	evRaw, err := json.Marshal(evs)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(evRaw, []byte(secret)) {
		t.Fatalf("settings 事件带出了秘密值: %s", evRaw)
	}
	list, msg := callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{})
	if msg != "" {
		t.Fatalf("list_settings 失败: %s", msg)
	}
	listRaw, err := json.Marshal(list)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(listRaw, []byte(secret)) {
		t.Fatalf("list_settings 回显了秘密值: %s", listRaw)
	}

	// 重置秘密项 = 推空值（服务自己清本地那份），中心仍不留值
	if _, msg := callSettings(t, wsURL, "reset_setting", protocol.ResetSettingPayload{
		Service: "svc-s", Key: "api.key",
	}); msg != "" {
		t.Fatalf("reset_setting 秘密项失败: %s", msg)
	}
	waitConfigure(t, svc, "api.key", "", 3*time.Second)
	waitSecretSet(t, wsURL, "svc-s", "api.key", false, 3*time.Second)

	// 只读项：面板看得到启动值，改它/重置它都被拒
	v = findSetting(t, list, "svc-s", "api.addr")
	if !v.ReadOnly || v.Value != "127.0.0.1:9101" || v.Overridden {
		t.Fatalf("只读项视图不符: %+v", v)
	}
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{
		Service: "svc-s", Key: "api.addr", Value: "0.0.0.0:1",
	}); msg == "" {
		t.Fatal("改只读项应被拒")
	} else if !strings.Contains(msg, "只读") {
		t.Fatalf("拒绝理由应说明只读: %s", msg)
	}
	if _, msg := callSettings(t, wsURL, "reset_setting", protocol.ResetSettingPayload{
		Service: "svc-s", Key: "api.addr",
	}); msg == "" {
		t.Fatal("重置只读项应被拒")
	}
	// 被拒之后中心盘上没有它的痕迹（普通项在，只读项不在）
	raw, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("api.addr")) || bytes.Contains(raw, []byte("0.0.0.0:1")) {
		t.Fatalf("只读项不该留下覆盖值: %s", raw)
	}
}
