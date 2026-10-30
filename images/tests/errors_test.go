package tests

// images 的失败路径：save 的参数/下载失败/大小上限，fetch 的空 id。
// 成功路径在 service_test.go；这里保证每种拒绝都有明确原因，而不是模糊的“失败”。

import (
	"context"
	"encoding/base64"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/images/server"
	"github.com/NouSart/sump/images/store"
	"github.com/NouSart/sump/protocol"
)

func startImagesForTest(t *testing.T, centerURL string) *server.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{
		Name: "images", Listen: "127.0.0.1:0", Center: centerURL,
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
	}, log.New(os.Stdout, "[images-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func TestSaveRejectsInvalidInput(t *testing.T) {
	sc := startStubCenter(t)
	srv := startImagesForTest(t, sc.URL())

	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{}); rp.OK || !strings.Contains(rp.Error, "url 或 data 必填") {
		t.Fatalf("空 payload 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: "!!!", Mime: "image/png"}); rp.OK || !strings.Contains(rp.Error, "base64") {
		t.Fatalf("非法 base64 应被拒且点明原因: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := call(t, srv.WsURL(), "fetch", protocol.ImageFetchPayload{}); rp.OK || !strings.Contains(rp.Error, "id 必填") {
		t.Fatalf("空 id 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
}

func TestSaveURLFailures(t *testing.T) {
	sc := startStubCenter(t)
	srv := startImagesForTest(t, sc.URL())

	// 200 但内容为空 → 明确报“图片为空”。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(empty.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: empty.URL + "/empty.png"}); rp.OK || !strings.Contains(rp.Error, "图片为空") {
		t.Fatalf("空内容应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}

	// 非 200 → 下载失败并带状态码。
	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	t.Cleanup(failing.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: failing.URL + "/x.png"}); rp.OK || !strings.Contains(rp.Error, "HTTP 500") {
		t.Fatalf("HTTP 500 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}

	// 大小上限：max_mb=1。边界要精确：恰好 1MiB 允许，1MiB+1 拒绝。
	if rp := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "max_mb", Value: "1", Default: "32", Overridden: true}); !rp.OK {
		t.Fatalf("设置 max_mb 失败: %s", rp.Error)
	}
	exact := make([]byte, 1<<20)
	exactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(exact)
	}))
	t.Cleanup(exactSrv.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: exactSrv.URL + "/exact.png"}); !rp.OK {
		t.Fatalf("恰好等于上限的下载应被接受: %s", rp.Error)
	}
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: base64.StdEncoding.EncodeToString(exact), Mime: "image/png"}); !rp.OK {
		t.Fatalf("恰好等于上限的 base64 应被接受: %s", rp.Error)
	}

	big := make([]byte, 1<<20+1)
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(big)
	}))
	t.Cleanup(large.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: large.URL + "/big.png"}); rp.OK || !strings.Contains(rp.Error, "下载内容超过") {
		t.Fatalf("超限下载应被下载阶段的守卫拒绝: ok=%v err=%q", rp.OK, rp.Error)
	}

	// 直接 base64 存入同样受上限约束。
	encoded := base64.StdEncoding.EncodeToString(big)
	if rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: encoded, Mime: "image/png"}); rp.OK || !strings.Contains(rp.Error, "上限") {
		t.Fatalf("超限 base64 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
}
