package tests

// save{url} 的 SSRF 守卫（files 侧）：与 images 共用 protocol/fetchsafe 实现，
// 这里钉「files 确实接上了这道闸」与两种状态在服务层都成立：
//   - 默认（生产）口径：http://127.0.0.1:xxxx 必须被拒——请求来自模型也不例外；
//   - 逃生闸开启（测试口径）：环回放行；非 http 协议与「重定向回环回」照旧被拒。

import (
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/files/server"
	"github.com/NouSart/sump/files/store"
	"github.com/NouSart/sump/protocol"
)

// startFilesGuard 按指定口径起 files：allowLoopback=false 即生产默认口径。
func startFilesGuard(t *testing.T, centerURL string, allowLoopback bool) *server.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{
		Name:               "files",
		Listen:             "127.0.0.1:0",
		Center:             centerURL,
		HeartbeatInterval:  200 * time.Millisecond,
		Store:              st,
		ReadMaxChars:       4000,
		FetchAllowLoopback: allowLoopback,
	}, log.New(os.Stdout, "[files-guard] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

func TestSaveGuardRejectsLoopbackByDefault(t *testing.T) {
	sc := startStubCenter(t)
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("secret"))
	}))
	t.Cleanup(content.Close)

	srv := startFilesGuard(t, sc.URL(), false)
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: content.URL + "/note.txt", Filename: "note.txt"})
	if rp.OK {
		t.Fatal("默认口径下 save 必须拒绝指向环回的 URL（模型可以编一个）")
	}
	if !strings.Contains(rp.Error, "下载被拒绝") || !strings.Contains(rp.Error, "环回") {
		t.Fatalf("拒绝文案应点明策略与对象（环回），实际: %q", rp.Error)
	}
}

func TestSaveGuardHatchAcceptsLoopback(t *testing.T) {
	sc := startStubCenter(t)
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("hello from httptest"))
	}))
	t.Cleanup(content.Close)

	srv := startFilesGuard(t, sc.URL(), true)
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: content.URL + "/note.txt", Filename: "note.txt"})
	if !rp.OK {
		t.Fatalf("开闸后对 httptest 对端的 save 应成功: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID == "" || sr.Filename != "note.txt" || sr.Size != int64(len("hello from httptest")) {
		t.Fatalf("save 结果不符: %+v", sr)
	}

	rp2 := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: "http://169.254.169.254/latest/meta-data/iam/security-credentials/", Filename: "creds"})
	if rp2.OK || !strings.Contains(rp2.Error, "链路本地") {
		t.Fatalf("逃生闸不该放行元数据地址，实际: ok=%v err=%q", rp2.OK, rp2.Error)
	}
}

func TestSaveGuardRejectsNonHTTPScheme(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFilesGuard(t, sc.URL(), true)
	for _, u := range []string{"file:///etc/passwd", "gopher://127.0.0.1:11211/x"} {
		rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: u, Filename: "x"})
		if rp.OK {
			t.Fatalf("非 http/https 协议 %q 必须被拒", u)
		}
		if !strings.Contains(rp.Error, "协议") {
			t.Fatalf("%q 的拒绝文案应点明协议，实际: %q", u, rp.Error)
		}
	}
}

func TestSaveGuardRejectsRedirectToLoopback(t *testing.T) {
	sc := startStubCenter(t)
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("internal-service-dump"))
	}))
	t.Cleanup(victim.Close)
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, victim.URL+"/dump.txt", http.StatusFound)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(relay.Close)

	srv := startFilesGuard(t, sc.URL(), true)
	rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: relay.URL + "/hop", Filename: "hop.txt"})
	if rp.OK {
		t.Fatal("重定向回环回必须被拒（逃生闸不覆盖重定向跳）")
	}
	if !strings.Contains(rp.Error, "重定向") || !strings.Contains(rp.Error, "环回") {
		t.Fatalf("拒绝文案应点明「重定向跳 + 环回」，实际: %q", rp.Error)
	}
}
