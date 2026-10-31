package tests

// images 服务级测试：真 WS + 真 SQLite/文件；save（URL 下载 / base64）→ fetch（base64 回转）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"image"
	"image/png"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/NouSart/sump/images/server"
	"github.com/NouSart/sump/images/store"
	"github.com/NouSart/sump/protocol"
)

// ---------- 设置中心替身（精简） ----------

type stubConn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func (c *stubConn) send(env protocol.Envelope) {
	data, err := protocol.Marshal(env)
	if err != nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_ = c.ws.WriteMessage(websocket.BinaryMessage, data)
}

type stubCenter struct {
	ln  net.Listener
	srv *http.Server

	mu   sync.Mutex
	regs []protocol.RegisterPayload // 收到的注册（断言名片声明用）
}

// Registers 返回收到的注册快照（并发安全）。
func (sc *stubCenter) Registers() []protocol.RegisterPayload {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return append([]protocol.RegisterPayload(nil), sc.regs...)
}

var stubUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func startStubCenter(t *testing.T) *stubCenter {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	sc := &stubCenter{ln: ln}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", sc.handleWS)
	sc.srv = &http.Server{Handler: mux}
	go func() { _ = sc.srv.Serve(ln) }()
	t.Cleanup(func() { _ = sc.srv.Close() })
	return sc
}

func (sc *stubCenter) URL() string { return "ws://" + sc.ln.Addr().String() + "/ws" }

func (sc *stubCenter) handleWS(w http.ResponseWriter, r *http.Request) {
	ws, err := stubUpgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	c := &stubConn{ws: ws}
	defer ws.Close()
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			return
		}
		env, err := protocol.Unmarshal(data)
		if err != nil {
			continue
		}
		switch env.Type {
		case protocol.TypeRegister:
			var reg protocol.RegisterPayload
			if err := env.DecodePayload(&reg); err == nil {
				sc.mu.Lock()
				sc.regs = append(sc.regs, reg)
				sc.mu.Unlock()
			}
			roster := protocol.RosterPayload{Services: []protocol.ServiceCard{{Name: "settings-center", Addr: sc.URL()}}, Revision: 1}
			respData, _ := protocol.EncodePayload(roster)
			resp, _ := protocol.NewResponse(env, "settings-center", protocol.ResponsePayload{OK: true, Data: respData})
			c.send(resp)
		case protocol.TypeHeartbeat:
			// 忽略
		}
	}
}

// ---------- 调用辅助 ----------

// call 向 images 发一次跳转并等回执（短连接）。
func call(t *testing.T, url, action string, input any) protocol.ResponsePayload {
	t.Helper()
	conn, err := protocol.Dial(url, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "test", "images", "trace-img", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	resp, err := conn.Call(ctx, env)
	if err != nil {
		t.Fatal(err)
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// ---------- 用例 ----------

// TestSaveFetchFlow 验证 save（URL 下载）→ fetch（base64 回转）与 base64 直存、错误场景。
func TestSaveFetchFlow(t *testing.T) {
	sc := startStubCenter(t)
	pngData := genPNG(t)
	fileSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write(pngData)
	}))
	t.Cleanup(fileSrv.Close)

	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	logger := log.New(os.Stdout, "[images-test] ", log.LstdFlags)
	srv := server.New(server.Config{
		Name: "images", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
	}, logger)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	// save by URL
	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{URL: fileSrv.URL + "/a.png"})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	if sr.ID == "" || sr.Mime != "image/png" || sr.Size != int64(len(pngData)) {
		t.Fatalf("save 结果不符: %+v", sr)
	}

	// fetch_full → base64 回转一致（服务间取原图；这个动作不给模型）
	rp2 := call(t, srv.WsURL(), "fetch_full", protocol.ImageFetchPayload{ID: sr.ID})
	if !rp2.OK {
		t.Fatalf("fetch 失败: %s", rp2.Error)
	}
	var fr protocol.ImageFetchResult
	if err := protocol.DecodeRaw(rp2.Data, &fr); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, pngData) || fr.Mime != "image/png" {
		t.Fatalf("fetch 内容不符: len=%d mime=%s", len(got), fr.Mime)
	}

	// save by base64
	rp3 := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{
		Data: base64.StdEncoding.EncodeToString(pngData),
	})
	if !rp3.OK {
		t.Fatalf("base64 save 失败: %s", rp3.Error)
	}

	// fetch 不存在的 id
	rp4 := call(t, srv.WsURL(), "fetch", protocol.ImageFetchPayload{ID: "不存在"})
	if rp4.OK {
		t.Fatal("不存在的 id 应报错")
	}
}

// TestFetchThumbnailAndWindow 钉住「模型看到的图有上限」：不给参数只回缩略图，给矩形回那块原图。
// 这里守的是上下文安全——一旦缩略图退化成整图，8MB 截图的 base64 会直接灌进模型。
func TestFetchThumbnailAndWindow(t *testing.T) {
	sc := startStubCenter(t)
	const bigW, bigH = 1600, 1200
	// 用噪声图当素材：PNG 压不动（≈真实截图的字节量级），才测得出「缩略图确实变小了」。
	imgBig := image.NewRGBA(image.Rect(0, 0, bigW, bigH))
	if _, err := rand.New(rand.NewSource(7)).Read(imgBig.Pix); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, imgBig); err != nil {
		t.Fatal(err)
	}
	bigPNG := buf.Bytes()

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

	rp := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: base64.StdEncoding.EncodeToString(bigPNG)})
	if !rp.OK {
		t.Fatalf("save 失败: %s", rp.Error)
	}
	var sr protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}

	fetch := func(t *testing.T, in protocol.ImageFetchPayload) protocol.ImageFetchResult {
		t.Helper()
		if in.ID == "" {
			in.ID = sr.ID
		}
		out := call(t, srv.WsURL(), "fetch", in)
		if !out.OK {
			t.Fatalf("fetch(%+v) 失败: %s", in, out.Error)
		}
		var r protocol.ImageFetchResult
		if err := protocol.DecodeRaw(out.Data, &r); err != nil {
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

	// 不给参数 = 缩略图：长边压到 768 以内，字节数远小于原图，并如实上报原始尺寸。
	th := fetch(t, protocol.ImageFetchPayload{})
	if th.Width > 768 || th.Height > 768 {
		t.Fatalf("缩略图超过 768: %dx%d", th.Width, th.Height)
	}
	if th.OriginalWidth != bigW || th.OriginalHeight != bigH {
		t.Fatalf("原始尺寸没上报: %dx%d", th.OriginalWidth, th.OriginalHeight)
	}
	if !th.Downscaled {
		t.Fatal("缩小了却没标 downscaled")
	}
	if th.DataBytes >= len(bigPNG) {
		t.Fatalf("缩略图没变小: %d >= %d", th.DataBytes, len(bigPNG))
	}

	// 给矩形 = 那块原图：尺寸原样、不放大不缩小、坐标如实回显。
	win := fetch(t, protocol.ImageFetchPayload{X1: 100, Y1: 100, X2: 300, Y2: 200})
	if win.X1 != 100 || win.Y1 != 100 || win.X2 != 300 || win.Y2 != 200 {
		t.Fatalf("窗口坐标没回显: %d,%d,%d,%d", win.X1, win.Y1, win.X2, win.Y2)
	}
	if win.Width != 200 || win.Height != 100 {
		t.Fatalf("窗口尺寸不符: %dx%d（期望 200x100）", win.Width, win.Height)
	}
	if win.Downscaled {
		t.Fatal("局部原图不该被缩小")
	}

	// 越界矩形夹到边界，而不是报错。
	edge := fetch(t, protocol.ImageFetchPayload{X1: bigW - 50, Y1: bigH - 40, X2: bigW + 500, Y2: bigH + 500})
	if edge.X2 != bigW || edge.Y2 != bigH || edge.Width != 50 || edge.Height != 40 {
		t.Fatalf("越界矩形没夹住: %d,%d %dx%d", edge.X2, edge.Y2, edge.Width, edge.Height)
	}

	// max_side 由调用方收紧。
	small := fetch(t, protocol.ImageFetchPayload{MaxSide: 100})
	if small.Width > 100 || small.Height > 100 {
		t.Fatalf("max_side=100 没生效: %dx%d", small.Width, small.Height)
	}

	// 反方向矩形报错，不静默返回整图。
	bad := call(t, srv.WsURL(), "fetch", protocol.ImageFetchPayload{ID: sr.ID, X1: 300, Y1: 300, X2: 100, Y2: 100})
	if bad.OK {
		t.Fatal("反向矩形应报错")
	}

	// fetch_full = 原图字节（服务间内联那条路），必须逐字节一致。
	rfp := call(t, srv.WsURL(), "fetch_full", protocol.ImageFetchPayload{ID: sr.ID})
	if !rfp.OK {
		t.Fatalf("fetch_full 失败: %s", rfp.Error)
	}
	var full protocol.ImageFetchResult
	if err := protocol.DecodeRaw(rfp.Data, &full); err != nil {
		t.Fatal(err)
	}
	rawFull, err := base64.StdEncoding.DecodeString(full.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawFull, bigPNG) || full.Mime != "image/png" {
		t.Fatalf("fetch_full 没回原图: len=%d mime=%s", len(rawFull), full.Mime)
	}

	// 小图不许被「压缩」得更大：缩略图比原图还大就原样发回。
	tinyPNG := genPNG(t)
	rpTiny := call(t, srv.WsURL(), "save", protocol.ImageSavePayload{Data: base64.StdEncoding.EncodeToString(tinyPNG)})
	if !rpTiny.OK {
		t.Fatalf("tiny save 失败: %s", rpTiny.Error)
	}
	var srTiny protocol.ImageSaveResult
	if err := protocol.DecodeRaw(rpTiny.Data, &srTiny); err != nil {
		t.Fatal(err)
	}
	tiny := fetch(t, protocol.ImageFetchPayload{ID: srTiny.ID})
	rawTiny, err := base64.StdEncoding.DecodeString(tiny.Data)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(rawTiny, tinyPNG) || tiny.Mime != "image/png" {
		t.Fatalf("2x2 小图被重编码了: len=%d（原 %d）mime=%s", len(rawTiny), len(tinyPNG), tiny.Mime)
	}
}

// TestToolExposure：images 是图片索引（存 → 按 id 转 base64 发回），save / fetch 对模型可用；
// fetch_full（原图，无大小上限）**不给模型**——声明错一个 flag，要么模型看不见能力，要么上下文被撑爆。
func TestToolExposure(t *testing.T) {
	sc := startStubCenter(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"), filepath.Join(t.TempDir(), "files"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	srv := server.New(server.Config{Name: "images", Listen: "127.0.0.1:0", Center: sc.URL(),
		HeartbeatInterval: 200 * time.Millisecond, Store: st}, log.New(os.Stdout, "[images-test] ", log.LstdFlags))
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && len(sc.Registers()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	tools := map[string]bool{}
	for _, reg := range sc.Registers() {
		for _, p := range reg.Provides {
			if p.Tool {
				tools[p.Action] = true
			}
		}
	}
	for _, want := range []string{"save", "fetch"} {
		if !tools[want] {
			t.Fatalf("images.%s 没暴露给模型（Tool: true 缺失）; 实际暴露=%v", want, tools)
		}
	}
	if tools["fetch_full"] {
		t.Fatalf("images.fetch_full 不该给模型（无上限原图会撑爆上下文）; 实际暴露=%v", tools)
	}
}
