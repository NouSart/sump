package tests

// 服务回传（report_setting）：服务侧的值自己变了 → 只更新「服务侧实际值」那一列，
// **不动**中心手里的覆盖值（两边都看得见）；秘密项只改「有没有值」；乱序旧报丢弃。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/settings-center/server"
)

// TestReportSetting 覆盖：值分两列、覆盖值不被回传改写、下发成功即视为一致、
// 乱序旧报丢弃、秘密只回标记、只读项也能回传、未声明键拒绝。
func TestReportSetting(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")
	s, err := server.Start(server.Config{Addr: "127.0.0.1:0", SettingsPath: path}, newLogger())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	wsURL := "ws://" + s.Addr() + "/ws"

	svc := startStubService(t)
	_, events := registerService(t, wsURL, "svc-r", svc.URL(), []protocol.Setting{
		{Key: "demo.mode", Default: "each"},
		{Key: "llm.key", Secret: true},
		{Key: "addr", Default: "127.0.0.1:9", ReadOnly: true},
	})

	// 服务侧自己变了：只落「服务侧」这一列
	res, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "demo.mode", Value: "self-changed", Seq: 2,
	})
	if msg != "" {
		t.Fatalf("report_setting 失败: %s", msg)
	}
	v := findSetting(t, res, "svc-r", "demo.mode")
	if v.Value != "each" || v.Overridden || v.Actual != "self-changed" || !v.Reported {
		t.Fatalf("回传后视图不符（中心口径不该被改写）: %+v", v)
	}
	// 结果以 settings 事件广播（面板据此实时刷新，不带值）
	evs := waitEventCause(t, events, "report_setting", 3*time.Second)
	found := false
	for _, e := range evs {
		if e.Cause == "report_setting" && e.Service == "svc-r" && len(e.Results) == 1 && e.Results[0].Key == "demo.mode" {
			if e.Results[0].Value != "" {
				t.Fatalf("回传事件不该带值: %+v", e.Results[0])
			}
			found = true
		}
	}
	if !found {
		t.Fatalf("没有 report_setting 事件: %+v", evs)
	}

	// 中心覆盖值照旧可写；写成功 = 已送达服务 → 服务侧实际值即该值（不该再报「服务侧已变」）
	if _, msg := callSettings(t, wsURL, "set_setting", protocol.SetSettingPayload{
		Service: "svc-r", Key: "demo.mode", Value: "batch",
	}); msg != "" {
		t.Fatalf("set_setting 失败: %s", msg)
	}
	waitConfigure(t, svc, "demo.mode", "batch", 3*time.Second)
	res, msg = callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "svc-r"})
	if msg != "" {
		t.Fatalf("list_settings 失败: %s", msg)
	}
	v = findSetting(t, res, "svc-r", "demo.mode")
	if v.Value != "batch" || !v.Overridden || v.Actual != "batch" {
		t.Fatalf("下发成功后两列应一致: %+v", v)
	}

	// 回传之间才有先后可言：先到 seq=3（新值）→ 旧报 seq=1 丢弃 → 更大的 seq=4 接受
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "demo.mode", Value: "newer", Seq: 3,
	}); msg != "" {
		t.Fatalf("report_setting 失败: %s", msg)
	}
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "demo.mode", Value: "stale", Seq: 1,
	}); msg != "" {
		t.Fatalf("report_setting 失败: %s", msg)
	}
	res, _ = callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "svc-r"})
	if v := findSetting(t, res, "svc-r", "demo.mode"); v.Actual != "newer" {
		t.Fatalf("乱序旧报应被丢弃: %+v", v)
	}
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "demo.mode", Value: "newest", Seq: 4,
	}); msg != "" {
		t.Fatalf("report_setting 失败: %s", msg)
	}
	res, _ = callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "svc-r"})
	if v := findSetting(t, res, "svc-r", "demo.mode"); v.Actual != "newest" {
		t.Fatalf("更大的 seq 应生效: %+v", v)
	}

	// 只读项也能回传（服务侧启动值与我们记的不一样时看得见）
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "addr", Value: "127.0.0.1:99", Seq: 1,
	}); msg != "" {
		t.Fatalf("只读项回传应被接受: %s", msg)
	}
	res, _ = callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "svc-r"})
	if v := findSetting(t, res, "svc-r", "addr"); !v.ReadOnly || v.Value != "127.0.0.1:9" || v.Actual != "127.0.0.1:99" {
		t.Fatalf("只读项两列不符: %+v", v)
	}

	// 秘密项：只认「有没有值」，值不上线也不落盘
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "llm.key", Set: true, Seq: 1,
	}); msg != "" {
		t.Fatalf("秘密回传应被接受: %s", msg)
	}
	res, _ = callSettings(t, wsURL, "list_settings", protocol.SettingsListPayload{Service: "svc-r"})
	if v := findSetting(t, res, "svc-r", "llm.key"); !v.Set || v.Value != "" || v.Actual != "" || v.Reported {
		t.Fatalf("秘密项回传后不符: %+v", v)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"llm.key", "self-changed", "newer"} {
		if strings.Contains(string(raw), leak) {
			t.Fatalf("回传不该落盘（含 %q）: %s", leak, raw)
		}
	}

	// 未声明的键：拒绝
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-r", Key: "ghost", Value: "x", Seq: 1,
	}); msg == "" {
		t.Fatal("未声明的键应被拒绝")
	}
	if _, msg := callSettings(t, wsURL, "report_setting", protocol.ReportSettingPayload{
		Service: "svc-x", Key: "demo.mode", Value: "x", Seq: 1,
	}); msg == "" {
		t.Fatal("未注册的服务应被拒绝")
	}
}
