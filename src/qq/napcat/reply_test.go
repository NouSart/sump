package napcat

// 动作响应的成功判定（replyFailure）是"消息到底送出去了没有"的唯一出口：
// 这些用例钉住 fail-closed——远端没明确说成功的响应一律不算成功。
// 网络侧只起本地的假 NapCat 端点（httptest + 127.0.0.1 的 WS 升级），不依赖外网。

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// ---------- 纯判定：响应形状 → 成功 / 失败 ----------

func TestReplyFailureShapes(t *testing.T) {
	cases := []struct {
		name     string
		body     string // 远端回的 JSON 帧（经真实解码路径进 map，字段类型与线上一致）
		wantOK   bool
		wantSubs []string // 失败时错误文本里必须出现的内容
	}{
		{
			name:   "ok_and_retcode",
			body:   `{"status":"ok","retcode":0,"data":{"message_id":1}}`,
			wantOK: true,
		},
		{
			name:   "retcode_only_ok", // 有的 NapCat 动作只回 retcode：这是真成功，不能拒
			body:   `{"retcode":0,"data":null}`,
			wantOK: true,
		},
		{
			name:   "status_only_ok",
			body:   `{"status":"ok"}`,
			wantOK: true,
		},
		{
			name:     "retcode_nonzero_without_status",
			body:     `{"retcode":1400,"message":"bad parameter"}`,
			wantSubs: []string{"send_msg", "retcode=1400", "bad parameter"},
		},
		{
			name:     "retcode_nonzero_with_ok_status", // retcode 说话，status 撒谎也不顶用
			body:     `{"status":"ok","retcode":100,"message":"stub send failure"}`,
			wantSubs: []string{"retcode=100", "stub send failure"},
		},
		{
			name:     "status_failed_retcode_zero", // 任何一个信号说失败就是失败
			body:     `{"status":"failed","retcode":0,"message":"offline"}`,
			wantSubs: []string{"status=\"failed\"", "offline"},
		},
		{
			name:     "no_signal_at_all", // 两个信号都不在：这条响应什么都没证明，不能算成功
			body:     `{"data":{"message_id":7}}`,
			wantSubs: []string{"status", "retcode"},
		},
		{
			name:     "status_not_a_string",
			body:     `{"status":500,"retcode":0}`,
			wantSubs: []string{"status=500（不是字符串）"},
		},
		{
			name:     "retcode_not_a_number",
			body:     `{"status":"ok","retcode":null}`,
			wantSubs: []string{"retcode=null（不是数字）"},
		},
		{
			name:     "wording_is_kept_too",
			body:     `{"status":"failed","retcode":2,"wording":"对方设置了不可添加"}`,
			wantSubs: []string{"status=\"failed\"", "retcode=2", "对方设置了不可添加"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var resp map[string]any
			if err := json.Unmarshal([]byte(tc.body), &resp); err != nil {
				t.Fatalf("用例 JSON 不合法: %v", err)
			}
			err := replyFailure("send_msg", resp)
			if tc.wantOK {
				if err != nil {
					t.Fatalf("该形状是真成功，却判成失败: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("畸形/失败响应 %s 被判成成功", tc.body)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("错误文本要含 %q，实际: %v", sub, err)
				}
			}
		})
	}
}

// TestReplyFailureOnEmptyBody 空响应体（解不出任何字段）不能算成功。
func TestReplyFailureOnEmptyBody(t *testing.T) {
	if err := replyFailure("get_group_info", nil); err == nil {
		t.Fatal("nil 响应体被判成成功")
	}
	if err := replyFailure("get_group_info", map[string]any{}); err == nil {
		t.Fatal("空响应体被判成成功")
	}
}

// ---------- 走真的 Client.Call：本地假 NapCat 端点 ----------

var fakeUpgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

// fakeNapCat 按 replyFn 给的原文回一帧（可以是"不是 JSON"的形状）。
type fakeNapCat struct {
	http    *httptest.Server
	mu      sync.Mutex
	replyFn func(action, echo string) string
}

// startFakeNapCat 起一个默认"明确成功"的假端点（形状可被 setReply 换掉）。
func startFakeNapCat(t *testing.T) *fakeNapCat {
	t.Helper()
	f := &fakeNapCat{replyFn: func(_, echo string) string {
		return `{"status":"ok","retcode":0,"echo":"` + echo + `"}`
	}}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		ws, err := fakeUpgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			_, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			var m map[string]any
			if json.Unmarshal(data, &m) != nil {
				continue
			}
			action, _ := m["action"].(string)
			echo, _ := m["echo"].(string)
			f.mu.Lock()
			fn := f.replyFn
			f.mu.Unlock()
			if err := ws.WriteMessage(websocket.TextMessage, []byte(fn(action, echo))); err != nil {
				return
			}
		}
	})
	f.http = httptest.NewServer(mux)
	t.Cleanup(f.http.Close)
	return f
}

func (f *fakeNapCat) URL() string {
	return "ws" + strings.TrimPrefix(f.http.URL, "http")
}

func (f *fakeNapCat) setReply(fn func(action, echo string) string) {
	f.mu.Lock()
	f.replyFn = fn
	f.mu.Unlock()
}

// callWith 连上假端点并调一次动作：未连上时重试（客户端的连接循环是异步的）。
func callWith(t *testing.T, c *Client, timeout time.Duration, action string) (map[string]any, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), timeout)
	defer cancel()
	return c.Call(ctx, action, map[string]any{"message": "hi"})
}

// startClient 起一个已连上假端点的客户端：先用一帧明确成功的响应试出来，
// 后面的用例才谈得上"响应到了、但形状认不出来"。
func startClient(t *testing.T, nc *fakeNapCat) *Client {
	t.Helper()
	c := New(Config{URL: nc.URL(), Logger: log.New(io.Discard, "", 0)})
	go c.Run(t.Context())
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := callWith(t, c, 500*time.Millisecond, "send_msg"); err == nil {
			return c
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("没连上假 NapCat")
	return nil
}

func TestCallFailClosedOverWS(t *testing.T) {
	cases := []struct {
		name     string
		frame    string // 远端回的整帧原文（%s 处填 echo）
		wantOK   bool
		wantSubs []string
	}{
		{
			name:   "ok_and_retcode_zero",
			frame:  `{"status":"ok","retcode":0,"echo":"%s"}`,
			wantOK: true,
		},
		{
			name:   "retcode_zero_without_status",
			frame:  `{"retcode":0,"data":{"message_id":9},"echo":"%s"}`,
			wantOK: true,
		},
		{
			name:     "retcode_nonzero",
			frame:    `{"retcode":100,"message":"stub send failure","echo":"%s"}`,
			wantSubs: []string{"send_msg", "retcode=100", "stub send failure"},
		},
		{
			name:     "neither_status_nor_retcode",
			frame:    `{"data":{},"echo":"%s"}`,
			wantSubs: []string{"send_msg", "status", "retcode"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nc := startFakeNapCat(t)
			c := startClient(t, nc)
			nc.setReply(func(_, echo string) string {
				return strings.Replace(tc.frame, "%s", echo, 1)
			})
			resp, err := callWith(t, c, 5*time.Second, "send_msg")
			if tc.wantOK {
				if err != nil {
					t.Fatalf("真成功的响应被判成失败: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("失败响应被报成成功: %+v", resp)
			}
			for _, sub := range tc.wantSubs {
				if !strings.Contains(err.Error(), sub) {
					t.Fatalf("错误文本要含 %q，实际: %v", sub, err)
				}
			}
		})
	}
}

// TestCallNonJSONFrameIsNeverSuccess 网关回的帧连 JSON 都不是（或不是对象）时，
// 它配不上 echo、客户端拿不到任何可认的响应——只能以错误收场，绝不能报"已送达"。
// 认不出来的帧无法归到某一次调用（帧里没有 echo），所以这里只能以超时结束；
// 断言的是底线"没有成功"，超时给短一点就够（真等不到也是 err）。
func TestCallNonJSONFrameIsNeverSuccess(t *testing.T) {
	for _, body := range []string{"not json at all", `[]`, `null`} {
		t.Run(body, func(t *testing.T) {
			nc := startFakeNapCat(t)
			c := startClient(t, nc)
			nc.setReply(func(_, _ string) string { return body })
			resp, err := callWith(t, c, 1*time.Second, "send_msg")
			if err == nil {
				t.Fatalf("非 JSON 响应 %q 被报成成功: %+v", body, resp)
			}
			if resp != nil {
				t.Fatalf("非 JSON 响应 %q 不该带回响应体: %+v", body, resp)
			}
		})
	}
}
