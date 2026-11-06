// settings-center 设置存取测试：声明 → 查询 → 覆盖 → 重置 → 重启后覆盖值仍在。
package tests

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
	"github.com/NouSart/sump/spv2"
)

// centerURL 返回设置中心的 SPv2 控制面地址。
func centerURL(s *server.Server) string { return "ws://" + s.Addr() + "/spv2" }

// dialCenter 连到设置中心，测试结束自动关闭。
func dialCenter(t *testing.T, url string, opts ...spv2.ClientOption) *spv2.Client {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := spv2.Dial(ctx, url, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// callCenter 在一条已建立的连接上调用中心的一个原生方法。
func callCenter(t *testing.T, c *spv2.Client, method string, input any, out any) error {
	t.Helper()
	f, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef("test"), spv2.ServiceRef("settings-center"), method, input)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.CallDecode(ctx, f, out)
}

// registerNative 通过原生 register 注册一个服务，返回中心回执里的名册。
func registerNative(t *testing.T, c *spv2.Client, name, addr, desc string, settings []protocol.Setting) protocol.RosterPayload {
	t.Helper()
	f, err := spv2.NewCall(spv2.NewID(), spv2.ServiceRef(name), spv2.ServiceRef("settings-center"), "register",
		protocol.RegisterPayload{Name: name, Addr: addr, Description: desc, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var roster protocol.RosterPayload
	if err := c.CallDecode(ctx, f, &roster); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	return roster
}

// registerWithSettings 注册一个声明了设置项的服务。
func registerWithSettings(t *testing.T, url, name string, items []protocol.Setting) {
	t.Helper()
	c := dialCenter(t, url)
	registerNative(t, c, name, "ws://127.0.0.1:1/spv2", "设置测试用服务", items)
}

// callSettings 发起一次设置动作（短连接），返回结果或错误文本。
func callSettings(t *testing.T, url, action string, input any) (protocol.SettingsResult, string) {
	t.Helper()
	c := dialCenter(t, url)
	var out protocol.SettingsResult
	if err := callCenter(t, c, action, input, &out); err != nil {
		return protocol.SettingsResult{}, err.Error()
	}
	return out, ""
}

// findSetting 在结果里取某服务的某一项（找不到直接失败）。
func findSetting(t *testing.T, res protocol.SettingsResult, service, key string) protocol.SettingView {
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
	t.Fatalf("结果中没有 %s.%s: %+v", service, key, res)
	return protocol.SettingView{}
}

// TestSettingsFlow 覆盖：声明默认值 → 查询 → 写入覆盖 → 过滤查询 → 校验 → 重置。
func TestSettingsFlow(t *testing.T) {
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0"}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	url := centerURL(s)

	registerWithSettings(t, url, "svc-a", []protocol.Setting{{Key: "demo.setting", Default: "5m"}})

	// 未覆盖：生效值 = 声明默认值
	res, errMsg := callSettings(t, url, "list_settings", protocol.SettingsListPayload{})
	if errMsg != "" {
		t.Fatalf("list_settings 失败: %s", errMsg)
	}
	if v := findSetting(t, res, "svc-a", "demo.setting"); v.Value != "5m" || v.Default != "5m" || v.Overridden {
		t.Fatalf("初始状态不符: %+v", v)
	}

	// 写入覆盖值
	res, errMsg = callSettings(t, url, "set_setting", protocol.SetSettingPayload{
		Service: "svc-a", Key: "demo.setting", Value: "10m",
	})
	if errMsg != "" {
		t.Fatalf("set_setting 失败: %s", errMsg)
	}
	if v := findSetting(t, res, "svc-a", "demo.setting"); v.Value != "10m" || !v.Overridden || v.Default != "5m" {
		t.Fatalf("覆盖状态不符: %+v", v)
	}

	// 按服务过滤
	res, errMsg = callSettings(t, url, "list_settings", protocol.SettingsListPayload{Service: "svc-a"})
	if errMsg != "" || len(res.Services) != 1 || res.Services[0].Service != "svc-a" {
		t.Fatalf("按服务过滤不符: %+v (%s)", res, errMsg)
	}

	// 未声明的项 / 未注册的服务 → 报错
	if _, msg := callSettings(t, url, "set_setting", protocol.SetSettingPayload{Service: "svc-a", Key: "不存在", Value: "x"}); msg == "" {
		t.Fatal("未声明的设置项应报错")
	}
	if _, msg := callSettings(t, url, "set_setting", protocol.SetSettingPayload{Service: "svc-x", Key: "k", Value: "x"}); msg == "" {
		t.Fatal("未注册的服务应报错")
	}

	// 重置 → 回落默认值
	res, errMsg = callSettings(t, url, "reset_setting", protocol.ResetSettingPayload{Service: "svc-a", Key: "demo.setting"})
	if errMsg != "" {
		t.Fatalf("reset_setting 失败: %s", errMsg)
	}
	if v := findSetting(t, res, "svc-a", "demo.setting"); v.Value != "5m" || v.Overridden {
		t.Fatalf("重置后状态不符: %+v", v)
	}
}

// TestSettingsPersistence 覆盖值落盘：重启设置中心后仍在；未再声明的项被清除。
func TestSettingsPersistence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	s1, err := server.Start(server.Config{Addr: "127.0.0.1:0", SettingsPath: path}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	url1 := centerURL(s1)
	registerWithSettings(t, url1, "svc-p", []protocol.Setting{
		{Key: "k1", Default: "d1"}, {Key: "k2", Default: "d2"},
	})
	if _, msg := callSettings(t, url1, "set_setting", protocol.SetSettingPayload{Service: "svc-p", Key: "k1", Value: "v1"}); msg != "" {
		t.Fatalf("set_setting 失败: %s", msg)
	}
	if err := s1.Shutdown(); err != nil {
		t.Fatal(err)
	}

	// 重启（同一文件）：覆盖值应被载入；只声明 k1 → k2 的痕迹应被清除
	s2, err := server.Start(server.Config{Addr: "127.0.0.1:0", SettingsPath: path}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Shutdown() })
	url2 := centerURL(s2)
	registerWithSettings(t, url2, "svc-p", []protocol.Setting{{Key: "k1", Default: "d1"}})

	res, errMsg := callSettings(t, url2, "list_settings", protocol.SettingsListPayload{})
	if errMsg != "" {
		t.Fatalf("list_settings 失败: %s", errMsg)
	}
	if v := findSetting(t, res, "svc-p", "k1"); v.Value != "v1" || !v.Overridden {
		t.Fatalf("覆盖值未持久化: %+v", v)
	}
	for _, ss := range res.Services {
		for _, v := range ss.Settings {
			if v.Key == "k2" {
				t.Fatalf("未再声明的项不应出现: %+v", v)
			}
		}
	}
}
