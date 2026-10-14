package tests

// files 设置项测试：max_mb / read_max_chars 下发即时生效（行为级）；
// 非法值与未声明键被拒、保持旧值。

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/files/server"
	"github.com/QAQ-awa-QAQ/sump/files/store"
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
	env, err := protocol.NewEnvelope(protocol.TypeJump, "settings-center", "files", "trace-cfg",
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

// startFilesCfg 起一个用于设置测试的 files 服务（存根中心 + 临时库）。
func startFilesCfg(t *testing.T, centerURL string) *server.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{
		Name: "files", Listen: "127.0.0.1:0", Center: centerURL,
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
	}, log.New(os.Stdout, "[files-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// saveText 存入一段文本并返回 id。
func saveText(t *testing.T, addr, filename, text string) string {
	t.Helper()
	rp := call(t, addr, "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString([]byte(text)),
		Filename: filename,
	})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	return sr.ID
}

// readFile 读取文件文本结果。
func readFile(t *testing.T, addr, id string) protocol.FileReadResult {
	t.Helper()
	rp := call(t, addr, "read", protocol.FileReadPayload{ID: id})
	if !rp.OK {
		t.Fatalf("read 失败: %s", rp.Error)
	}
	var rr protocol.FileReadResult
	if err := protocol.DecodeRaw(rp.Data, &rr); err != nil {
		t.Fatal(err)
	}
	return rr
}

// TestSettingsReadMaxChars 下发 read_max_chars=10 后读取截断到 10 字（truncated=true）；
// 非法值（<100）与未声明键被拒、保持旧值。
func TestSettingsReadMaxChars(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFilesCfg(t, sc.URL())

	full := "一二三四五六七八九十还有超出的部分"
	id := saveText(t, srv.WsURL(), "note.txt", full)

	// 默认阈值（4000）：全文、未截断。
	if rr := readFile(t, srv.WsURL(), id); rr.Text != full || rr.Truncated {
		t.Fatalf("默认阈值应读全文: %+v", rr)
	}

	// 下发 read_max_chars=10 → 即时生效。
	ok := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "read_max_chars", Value: "10", Default: "4000", Overridden: true})
	if !ok.OK {
		t.Fatalf("下发失败: %s", ok.Error)
	}
	if v := srv.SettingValue("read_max_chars"); v != "10" {
		t.Fatalf("生效值应为 10，实际 %q", v)
	}
	if rr := readFile(t, srv.WsURL(), id); !rr.Truncated || rr.Text != "一二三四五六七八九十" {
		t.Fatalf("下发后应截断到 10 字: %+v", rr)
	}

	// 非法值 / 未声明键：拒绝且保持旧值。
	bad := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "read_max_chars", Value: "0"})
	if bad.OK || !strings.Contains(bad.Error, "≥1") {
		t.Fatalf("非法值应被拒绝: %+v", bad)
	}
	if v := srv.SettingValue("read_max_chars"); v != "10" {
		t.Fatalf("拒绝后应保持 10，实际 %q", v)
	}
	ghost := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "ghost", Value: "1"})
	if ghost.OK || !strings.Contains(ghost.Error, "未声明的设置项") {
		t.Fatalf("未声明键应被拒绝: %+v", ghost)
	}
}

// TestSettingsMaxMB 下发 max_mb=1 后，1.5MiB 的文件保存被拒（错误含新上限字节数）。
func TestSettingsMaxMB(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFilesCfg(t, sc.URL())

	big := base64.StdEncoding.EncodeToString(make([]byte, 1<<20+(512<<10))) // 1.5MiB

	// 默认上限（100MiB）：可保存。
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{Data: big, Filename: "big.bin"}); !rp.OK {
		t.Fatalf("默认上限内应可保存: %s", rp.Error)
	}

	// 下发 max_mb=1 → 立即生效。
	ok := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "max_mb", Value: "1", Default: "100", Overridden: true})
	if !ok.OK {
		t.Fatalf("下发失败: %s", ok.Error)
	}
	if v := srv.SettingValue("max_mb"); v != "1" {
		t.Fatalf("生效值应为 1，实际 %q", v)
	}
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{Data: big, Filename: "big.bin"})
	if rp.OK || !strings.Contains(rp.Error, "1048576") {
		t.Fatalf("超限保存应被拒（错误应含新上限 1048576）: %+v", rp)
	}

	// 非法值：拒绝且保持旧值。
	bad := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "max_mb", Value: "0"})
	if bad.OK || !strings.Contains(bad.Error, "≥1") {
		t.Fatalf("非法值应被拒绝: %+v", bad)
	}
	if v := srv.SettingValue("max_mb"); v != "1" {
		t.Fatalf("拒绝后应保持 1，实际 %q", v)
	}
}
