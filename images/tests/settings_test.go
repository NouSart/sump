// images 设置项测试：max_mb 下发即时生效（超限保存被拒）；非法值与未声明键被拒、保持旧值。
package tests

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/images/server"
	"github.com/QAQ-awa-QAQ/sump/images/store"
	"github.com/QAQ-awa-QAQ/sump/protocol"
)

// pushConfigure 以设置中心身份向服务地址推送一条 configure（单键）并取回执。
func pushConfigure(t *testing.T, addr string, p protocol.ConfigurePayload) protocol.ResponsePayload {
	t.Helper()
	raw, err := protocol.EncodePayload(p)
	if err != nil {
		t.Fatal(err)
	}
	c, err := protocol.Dial(addr, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	env, err := protocol.NewEnvelope(protocol.TypeJump, "settings-center", "images", "trace-cfg",
		protocol.JumpPayload{Action: "configure", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resp, err := c.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// TestSettingsMaxMB 验证：下发 max_mb=1 后，1.5MiB 的图片保存被拒（错误含新上限字节数）；
// 非法值与未声明键被拒且保持旧值。
func TestSettingsMaxMB(t *testing.T) {
	sc := startStubCenter(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{
		Name: "images", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
	}, log.New(os.Stdout, "[images-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	big := base64.StdEncoding.EncodeToString(make([]byte, 1<<20+(512<<10))) // 1.5MiB

	// 默认上限（32MiB）：可保存。
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: big, Mime: "image/png"}); !rp.OK {
		t.Fatalf("默认上限内应可保存: %s", rp.Error)
	}

	// 下发 max_mb=1 → 立即生效。
	ok := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "max_mb", Value: "1", Default: "32", Overridden: true})
	if !ok.OK {
		t.Fatalf("下发失败: %s", ok.Error)
	}
	if v := srv.SettingValue("max_mb"); v != "1" {
		t.Fatalf("生效值应为 1，实际 %q", v)
	}
	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: big, Mime: "image/png"})
	if rp.OK || !strings.Contains(rp.Error, "1048576") {
		t.Fatalf("超限保存应被拒（错误应含新上限 1048576）: %+v", rp)
	}

	// 非法值 / 未声明键：拒绝且保持旧值。
	bad := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "max_mb", Value: "0"})
	if bad.OK || !strings.Contains(bad.Error, "≥1") {
		t.Fatalf("非法值应被拒绝: %+v", bad)
	}
	if v := srv.SettingValue("max_mb"); v != "1" {
		t.Fatalf("拒绝后应保持 1，实际 %q", v)
	}
	ghost := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "ghost", Value: "1"})
	if ghost.OK || !strings.Contains(ghost.Error, "未声明的设置项") {
		t.Fatalf("未声明键应被拒绝: %+v", ghost)
	}
}
