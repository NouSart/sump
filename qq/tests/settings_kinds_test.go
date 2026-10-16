// qq 的三类设置项边界：只读项（引导三要素）改动被拒、秘密项（napcat.token）值只在服务侧、
// napcat.url 热改会真的重连到新端点。
package tests

import (
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

func TestSettingsKindsAndHotURL(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	// 只读项：启动参数带起来的，改动一律被明确拒绝
	for _, key := range []string{"name", "addr", "center"} {
		rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: key, Value: "x"})
		if rp.OK || !strings.Contains(rp.Error, "只读") {
			t.Fatalf("%s 是只读项，应被拒绝: %+v", key, rp)
		}
	}

	// 秘密项：服务侧持有（"不进中心"由骨架测试保证），空值 = 清除
	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "napcat.token", Value: "tok-1"}); !rp.OK {
		t.Fatalf("秘密下发应成功: %+v", rp)
	}
	if v := qq.SettingValue("napcat.token"); v != "tok-1" {
		t.Fatalf("服务侧应持有秘密，实际 %q", v)
	}
	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "napcat.token", Value: ""}); !rp.OK {
		t.Fatalf("清空秘密应成功: %+v", rp)
	}
	if v := qq.SettingValue("napcat.token"); v != "" {
		t.Fatalf("清空后生效值应为空，实际 %q", v)
	}

	// napcat.url 热改：换成第二个替身 → 新端点应收到连接（旧连接让位）
	nc2 := startStubNapCat(t)
	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "napcat.url", Value: nc2.URL()}); !rp.OK {
		t.Fatalf("napcat.url 应可热改: %+v", rp)
	}
	nc2.waitConn(t, 8*time.Second)
	if v := qq.SettingValue("napcat.url"); v != nc2.URL() {
		t.Fatalf("生效值应为新端点，实际 %q", v)
	}

	// 非法地址：拒绝且保持新端点
	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "napcat.url", Value: "http://x"}); rp.OK {
		t.Fatalf("非法 URL 应被拒: %+v", rp)
	}
	if v := qq.SettingValue("napcat.url"); v != nc2.URL() {
		t.Fatalf("拒绝后应保持旧值，实际 %q", v)
	}
}

// TestSettingsFilesHotChange 验证：files 是普通项——热改后文件消息真的存到新服务。
// （files 是本次接入的新配置项，不能只声明不验。）
func TestSettingsFilesHotChange(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	filesA := startStubFilesNamed(t, sc, "files")
	filesB := startStubFilesNamed(t, sc, "files-b")
	startStubImages(t, sc)
	startStubReasoner(t, sc)
	qq := startQQ(t, sc, nc, "2271917353")
	nc.waitConn(t, 5*time.Second)

	// 热改前：文件存到 files
	nc.sendEvent(t, privateFileEvent(2271917353, "看看", "http://stub/a.md", "a.md"))
	filesA.waitSave(t, 5*time.Second)
	beforeB := filesB.savesLen()

	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "files", Value: "files-b", Default: "files", Overridden: true}); !rp.OK {
		t.Fatalf("files 应可热改: %+v", rp)
	}
	if v := qq.SettingValue("files"); v != "files-b" {
		t.Fatalf("生效值应为 files-b，实际 %q", v)
	}

	// 热改后：文件应存到 files-b
	nc.sendEvent(t, privateFileEvent(2271917353, "再看看", "http://stub/b.md", "b.md"))
	filesB.waitSave(t, 5*time.Second)
	if filesB.savesLen() <= beforeB {
		t.Fatal("热改后文件应存到新服务")
	}

	// 空服务名：拒绝
	if rp := pushConfigure(t, qq.WsURL(), protocol.ConfigurePayload{Key: "files", Value: "  "}); rp.OK {
		t.Fatal("空服务名应被拒绝")
	}
}
