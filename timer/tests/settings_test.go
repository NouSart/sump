// timer 的设置项：portal（热改，决定快照推给谁）与只读三要素。
package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
	"github.com/QAQ-awa-QAQ/sump/timer/server"
)

// TestSettingsKindsAndPortalHotChange 验证：
// 只读项（name/addr/center）改动被拒；timer.portal 热改后快照真的推给新门户（行为断言）；
// 清空 = 不推；名片里三类声明各就各位。
func TestSettingsKindsAndPortalHotChange(t *testing.T) {
	sc := startStubCenter(t)
	portalA := startPortalStub(t, sc, "portal-a")
	portalB := startPortalStub(t, sc, "portal-b")
	srv := startTimer(t, sc, server.Config{Portal: "portal-a", MaxDuration: time.Hour, MaxPending: 64})
	startCaller(t, sc, "e2e-caller")
	cl := dial(t, "ws://"+listenAddr(t, sc))

	set := func(key, value string) protocol.ResponsePayload {
		return request(t, cl, "e2e-caller", "configure", protocol.ConfigurePayload{Key: key, Value: value})
	}
	sleep := func(note string) protocol.ResponsePayload {
		return request(t, cl, "e2e-caller", "sleep", protocol.TimerSleepPayload{MS: 60000, Note: note})
	}

	// 只读项：改动一律被明确拒绝
	for _, key := range []string{"name", "addr", "center"} {
		if rp := set(key, "x"); rp.OK || !strings.Contains(rp.Error, "只读") {
			t.Fatalf("%s 是只读项，应被拒绝: %+v", key, rp)
		}
	}

	// 先让 portal-a 收到一条快照（受理一条计时）
	if rp := sleep("热改前"); !rp.OK {
		t.Fatalf("sleep 应被受理: %s", rp.Error)
	}
	portalA.waitState(t, 1, 5*time.Second)
	beforeB := portalB.snapCount()

	// 热改 portal → portal-b：下一次状态变化应推给 portal-b
	if rp := set("portal", "portal-b"); !rp.OK {
		t.Fatalf("portal 应可热改: %+v", rp)
	}
	if v := srv.SettingValue("portal"); v != "portal-b" {
		t.Fatalf("生效值应为 portal-b，实际 %q", v)
	}
	if rp := sleep("热改后"); !rp.OK {
		t.Fatalf("sleep 应被受理: %s", rp.Error)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && portalB.snapCount() == beforeB {
		time.Sleep(20 * time.Millisecond)
	}
	if portalB.snapCount() == beforeB {
		t.Fatal("热改后快照应推给新门户")
	}

	// 清空 = 不推（再受理一条，两边都不该增加）
	if rp := set("portal", ""); !rp.OK {
		t.Fatalf("清空 portal 应被接受: %+v", rp)
	}
	if v := srv.SettingValue("portal"); v != "" {
		t.Fatalf("清空后生效值应为空，实际 %q", v)
	}
	nA, nB := portalA.snapCount(), portalB.snapCount()
	if rp := sleep("关掉推送后"); !rp.OK {
		t.Fatalf("sleep 应被受理: %s", rp.Error)
	}
	time.Sleep(400 * time.Millisecond)
	if portalA.snapCount() != nA || portalB.snapCount() != nB {
		t.Fatalf("关掉推送后不该再推快照: A %d→%d, B %d→%d", nA, portalA.snapCount(), nB, portalB.snapCount())
	}

	// 名片里三类都在（portal 普通项 + 三要素只读 + 既有两项不丢）
	byKey := map[string]protocol.Setting{}
	for _, it := range sc.DeclaredSettings("timer") {
		byKey[it.Key] = it
	}
	if it := byKey["portal"]; it.Key == "" || it.ReadOnly || it.Secret {
		t.Fatalf("portal 应是普通项: %+v", it)
	}
	for _, key := range []string{"name", "addr", "center"} {
		if it := byKey[key]; !it.ReadOnly {
			t.Fatalf("%s 应是只读项: %+v", key, it)
		}
	}
	if byKey["max_ms"].Key == "" || byKey["max_pending"].Key == "" {
		t.Fatalf("既有设置项不该丢: %+v", byKey)
	}
}
