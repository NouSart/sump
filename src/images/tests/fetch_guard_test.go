package tests

// save{url} 的 SSRF 守卫与 mime 诚实性（images 侧）：
//   - 默认（生产）口径：指向 127.0.0.1 的 URL 必须被拒——即使请求来自模型；
//   - 逃生闸开启（测试口径）：环回放行，但非 http 协议、重定向回环回照旧被拒；
//   - fetch 返回的 mime 必须与 base64  payload 的实际魔数一致（PNG 字节不得标 JPEG）。
//
// 守卫本体的细粒度表驱动测试在 protocol/fetchsafe（跨服务共用实现）；这里钉的是
// 「images 确实接上了这道闸」与「闸的两种状态在服务层都成立」。

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"log"
	"math/rand"
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

// startImagesGuard 按指定口径起 images：allowLoopback=false 即生产默认口径。
func startImagesGuard(t *testing.T, centerURL string, allowLoopback bool) *server.Server {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{
		Name:               "images",
		Listen:             "127.0.0.1:0",
		Center:             centerURL,
		HeartbeatInterval:  200 * time.Millisecond,
		Store:              st,
		FetchAllowLoopback: allowLoopback,
	}, log.New(os.Stdout, "[images-guard] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	return srv
}

// TestSaveGuardRejectsLoopbackByDefault 钉住默认口径：模型编一个 http://127.0.0.1:xxxx
// 也读不到本机服务——文案必须点明环回被拒。
func TestSaveGuardRejectsLoopbackByDefault(t *testing.T) {
	sc := startStubCenter(t)
	pngData := genPNG(t)
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	t.Cleanup(content.Close)

	srv := startImagesGuard(t, sc.URL(), false)
	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: content.URL + "/a.png"})
	if rp.OK {
		t.Fatal("默认口径下 save 必须拒绝指向环回的 URL（模型可以编一个）")
	}
	if !strings.Contains(rp.Error, "下载被拒绝") || !strings.Contains(rp.Error, "环回") {
		t.Fatalf("拒绝文案应点明策略与对象（环回），实际: %q", rp.Error)
	}
}

// TestSaveGuardHatchAcceptsLoopback 钉住逃生闸的另一半：开闸后（测试口径）环回可下载，
// 且闸只覆盖环回——云元数据地址照旧被拒。
func TestSaveGuardHatchAcceptsLoopback(t *testing.T) {
	sc := startStubCenter(t)
	pngData := genPNG(t)
	content := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	t.Cleanup(content.Close)

	srv := startImagesGuard(t, sc.URL(), true)
	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: content.URL + "/a.png"})
	if !rp.OK {
		t.Fatalf("开闸后对 httptest 对端的 save 应成功: %s", rp.Error)
	}
	var sr protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID == "" || sr.Size != int64(len(pngData)) {
		t.Fatalf("save 结果不符: %+v", sr)
	}

	rp2 := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: "http://169.254.169.254/latest/meta-data/iam/security-credentials/"})
	if rp2.OK || !strings.Contains(rp2.Error, "链路本地") {
		t.Fatalf("逃生闸不该放行元数据地址，实际: ok=%v err=%q", rp2.OK, rp2.Error)
	}
}

// TestSaveGuardRejectsNonHTTPScheme 钉第一层（协议白名单）：file:///etc/passwd 这类
// 输入必须被拒且点明协议问题——开闸与否都拦（闸只管环回，不管协议）。
func TestSaveGuardRejectsNonHTTPScheme(t *testing.T) {
	sc := startStubCenter(t)
	srv := startImagesGuard(t, sc.URL(), true)
	for _, u := range []string{"file:///etc/passwd", "gopher://127.0.0.1:11211/x"} {
		rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: u})
		if rp.OK {
			t.Fatalf("非 http/https 协议 %q 必须被拒", u)
		}
		if !strings.Contains(rp.Error, "协议") {
			t.Fatalf("%q 的拒绝文案应点明协议，实际: %q", u, rp.Error)
		}
	}
}

// TestSaveGuardRejectsRedirectToLoopback 钉第三层（逐跳复查）：闸内的首个地址被 302
// 带回环回另一个服务时，重定向跳按严格口径拒绝——「公开（闸内）中转页 → 本机服务」这条
// SSRF 经典链必须断在第 1 跳的检查上。
func TestSaveGuardRejectsRedirectToLoopback(t *testing.T) {
	sc := startStubCenter(t)
	// 伪装成「内部敏感服务」的对端：谁连上它就中招。
	victim := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(genPNG(t))
	}))
	t.Cleanup(victim.Close)
	// 首个地址是闸内环回（下载入口合法），但它 302 到另一个环回服务。
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/hop" {
			http.Redirect(w, r, victim.URL+"/secret.png", http.StatusFound)
			return
		}
		http.Error(w, "not found", http.StatusNotFound)
	}))
	t.Cleanup(relay.Close)

	srv := startImagesGuard(t, sc.URL(), true)
	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: relay.URL + "/hop"})
	if rp.OK {
		t.Fatal("重定向回环回必须被拒（逃生闸不覆盖重定向跳）")
	}
	if !strings.Contains(rp.Error, "重定向") || !strings.Contains(rp.Error, "环回") {
		t.Fatalf("拒绝文案应点明「重定向跳 + 环回」，实际: %q", rp.Error)
	}
}

// TestFetchMimeMatchesActualBytes 钉 mime 指鹿为马的修复：PNG 源与 JPEG 源分别走
// 重编码路径，返回的 mime 必须与 base64 payload 的魔数一致。
// 只看 mime 字符串不算数——必须解码后验魔数，未来谁再硬编码 mime 就当场翻车。
func TestFetchMimeMatchesActualBytes(t *testing.T) {
	sc := startStubCenter(t)
	srv := startImagesGuard(t, sc.URL(), false) // 本用例不下载 URL，用默认口径顺带证明「不走闸也能存取」

	fetch := func(id string) protocol.ImageFetchResult {
		t.Helper()
		rp := call(t, srv.WsURL(), "fetch", protocol.ImageFetchPayload{ID: id})
		if !rp.OK {
			t.Fatalf("fetch(%s) 失败: %s", id, rp.Error)
		}
		var r protocol.ImageFetchResult
		if err := protocol.DecodeRaw(rp.Data, &r); err != nil {
			t.Fatal(err)
		}
		raw, err := base64.StdEncoding.DecodeString(r.Data)
		if err != nil {
			t.Fatal(err)
		}
		if len(raw) != r.DataBytes {
			t.Fatalf("DataBytes=%d 与实际解码 %d 不符", r.DataBytes, len(raw))
		}
		return r
	}

	// —— PNG 源（噪声大图：必走缩略图重编码，绕开「小图原样发回」的逃逸分支）——
	bigPNG := noisePNG(t, 1600, 1200)
	rpPNG := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: base64.StdEncoding.EncodeToString(bigPNG), Mime: "image/png"})
	if !rpPNG.OK {
		t.Fatalf("PNG save 失败: %s", rpPNG.Error)
	}
	var srPNG protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rpPNG.Data, &srPNG); err != nil {
		t.Fatal(err)
	}
	thPNG := fetch(srPNG.ID)
	if thPNG.Mime != "image/png" {
		t.Fatalf("PNG 源重编码产物被标成 %q（指鹿为马）", thPNG.Mime)
	}
	raw, _ := base64.StdEncoding.DecodeString(thPNG.Data)
	if !bytes.HasPrefix(raw, []byte("\x89PNG\r\n\x1a\n")) {
		t.Fatalf("mime=image/png 但魔数不是 PNG: % x", raw[:8])
	}

	// —— JPEG 源 ——
	jpegData := noiseJPEG(t, 1600, 1200)
	rpJ := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: base64.StdEncoding.EncodeToString(jpegData), Mime: "image/jpeg"})
	if !rpJ.OK {
		t.Fatalf("JPEG save 失败: %s", rpJ.Error)
	}
	var srJ protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rpJ.Data, &srJ); err != nil {
		t.Fatal(err)
	}
	thJ := fetch(srJ.ID)
	if thJ.Mime != "image/jpeg" {
		t.Fatalf("JPEG 源产物 mime 不符: %q", thJ.Mime)
	}
	rawJ, _ := base64.StdEncoding.DecodeString(thJ.Data)
	if !bytes.HasPrefix(rawJ, []byte{0xFF, 0xD8, 0xFF}) {
		t.Fatalf("mime=image/jpeg 但魔数不是 JPEG: % x", rawJ[:3])
	}
}

// noisePNG 造一张不可压缩的大噪声 PNG（保证 fetch 走重编码路径而不是小图原样发回）。
func noisePNG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := rand.New(rand.NewSource(11)).Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// noiseJPEG 造一张大噪声 JPEG（同样为了绕开小图逃逸分支）。
func noiseJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	if _, err := rand.New(rand.NewSource(13)).Read(img.Pix); err != nil {
		t.Fatal(err)
	}
	// 保证 alpha 全不透明，编码器对 A=0 的表现不作要求。
	for i := 3; i < len(img.Pix); i += 4 {
		img.Pix[i] = 255
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
