package tests

// files 的失败路径：save 的参数/下载失败/大小上限，fetch/read/write/edit_* 的必填字段。
// 格式级错误（docx/xlsx/pptx）在各自的 *_test.go 里；这里补齐“入口校验”。

import (
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestSaveRejectsInvalidInput(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)

	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{}); rp.OK || !strings.Contains(rp.Error, "url 或 data 必填") {
		t.Fatalf("空 payload 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{Data: "!!!", Filename: "x.txt"}); rp.OK || !strings.Contains(rp.Error, "base64") {
		t.Fatalf("非法 base64 应被拒且点明原因: ok=%v err=%q", rp.OK, rp.Error)
	}

	// 200 但内容为空 → 明确报“文件为空”。
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(empty.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: empty.URL + "/empty.txt", Filename: "empty.txt"}); rp.OK || !strings.Contains(rp.Error, "文件为空") {
		t.Fatalf("空内容应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}

	failing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusBadGateway)
	}))
	t.Cleanup(failing.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: failing.URL + "/x.txt", Filename: "x.txt"}); rp.OK || !strings.Contains(rp.Error, "HTTP 502") {
		t.Fatalf("HTTP 502 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}

	// 大小上限：max_mb=1。边界要精确：恰好 1MiB 允许，1MiB+1 拒绝。
	if rp := pushConfigure(t, srv.WsURL(), protocol.ConfigurePayload{Key: "max_mb", Value: "1", Default: "32", Overridden: true}); !rp.OK {
		t.Fatalf("设置 max_mb 失败: %s", rp.Error)
	}
	exact := make([]byte, 1<<20)
	exactSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(exact)
	}))
	t.Cleanup(exactSrv.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: exactSrv.URL + "/exact.bin", Filename: "exact.bin"}); !rp.OK {
		t.Fatalf("恰好等于上限的下载应被接受: %s", rp.Error)
	}
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{Data: base64.StdEncoding.EncodeToString(exact), Filename: "exact.bin"}); !rp.OK {
		t.Fatalf("恰好等于上限的 base64 应被接受: %s", rp.Error)
	}

	big := make([]byte, 1<<20+1)
	large := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(big)
	}))
	t.Cleanup(large.Close)
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{URL: large.URL + "/big.bin", Filename: "big.bin"}); rp.OK || !strings.Contains(rp.Error, "下载内容超过") {
		t.Fatalf("超限下载应被下载阶段的守卫拒绝: ok=%v err=%q", rp.OK, rp.Error)
	}
	if rp := call(t, srv.WsURL(), "save", protocol.FileSavePayload{Data: base64.StdEncoding.EncodeToString(big), Filename: "big.bin"}); rp.OK || !strings.Contains(rp.Error, "上限") {
		t.Fatalf("超限 base64 应被拒: ok=%v err=%q", rp.OK, rp.Error)
	}
}

func TestRequiredIDsAndEdits(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)

	cases := []struct {
		name   string
		action string
		input  any
		want   string
	}{
		{"fetch_without_id", "fetch", protocol.FileFetchPayload{}, "fetch: id 必填"},
		{"read_without_id", "read", protocol.FileReadPayload{}, "read: id 必填"},
		{"write_without_id", "write", protocol.FileWritePayload{}, "write: id 必填"},
		{"edit_word_without_id", "edit_word", protocol.FileWordEditPayload{}, "edit_word: id 必填"},
		{"edit_ppt_without_id", "edit_ppt", protocol.FilePptEditPayload{}, "edit_ppt: id 必填"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rp := call(t, srv.WsURL(), tc.action, tc.input)
			if rp.OK || !strings.Contains(rp.Error, tc.want) {
				t.Fatalf("%s 应被拒并说明 %q: ok=%v err=%q", tc.action, tc.want, rp.OK, rp.Error)
			}
		})
	}
}
