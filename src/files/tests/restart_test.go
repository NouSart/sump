package tests

// 完整服务重启实验：同一数据库 / 二进制目录，关闭服务和 Store 后重新启动。
// 与"只重开 Store"不同，这条路径覆盖服务生命周期、重新注册和重新打开权威副本。

import (
	"bytes"
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/files/server"
	"github.com/NouSart/sump/files/store"
	"github.com/NouSart/sump/protocol"
)

// startFilesPersistent 在指定 db/dir 上启动 files，并返回一次性关闭函数。
func startFilesPersistent(t *testing.T, centerURL, dbPath, dir string) (*server.Server, func()) {
	t.Helper()
	st, err := store.Open(dbPath, dir)
	if err != nil {
		t.Fatalf("打开持久库失败: %v", err)
	}
	srv := server.New(server.Config{
		Name: "files", Listen: "127.0.0.1:0", Center: centerURL,
		HeartbeatInterval: 200 * time.Millisecond, Store: st,
		ReadMaxChars: 4000, DBPath: dbPath, Dir: dir,
	}, log.New(os.Stdout, "[files-restart] ", log.LstdFlags))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Start(ctx); err != nil {
		_ = st.Close()
		t.Fatalf("启动 files 失败: %v", err)
	}
	var closed bool
	closeFn := func() {
		if closed {
			return
		}
		closed = true
		srv.Shutdown()
		if err := st.Close(); err != nil {
			t.Errorf("关闭 Store 失败: %v", err)
		}
	}
	t.Cleanup(closeFn)
	return srv, closeFn
}

func TestFilesServiceRestartKeepsAuthority(t *testing.T) {
	sc := startStubCenter(t)
	root := t.TempDir()
	dbPath := filepath.Join(root, "meta", "files.db")
	dir := filepath.Join(root, "blobs")

	srv1, close1 := startFilesPersistent(t, sc.URL(), dbPath, dir)

	xlsxID := saveXlsx(t, srv1.WsURL(), "持久.xlsx", makeXlsx(t))
	if rp := call(t, srv1.WsURL(), "write", protocol.FileWritePayload{
		ID: xlsxID, Sheet: "Sheet1",
		Edits: []protocol.FileWriteEdit{{Cell: "B2", Value: "重启前写入"}},
	}); !rp.OK {
		t.Fatalf("重启前编辑失败: %s", rp.Error)
	}
	textID := saveText(t, srv1.WsURL(), "持久.txt", "重启后仍可读")
	before := fetchBytes(t, srv1.WsURL(), xlsxID)

	close1()

	srv2, _ := startFilesPersistent(t, sc.URL(), dbPath, dir)

	after := fetchBytes(t, srv2.WsURL(), xlsxID)
	if !bytes.Equal(before, after) {
		t.Fatalf("重启后 xlsx 二进制变化: %d → %d 字节", len(before), len(after))
	}
	rr := readFile(t, srv2.WsURL(), textID)
	if rr.Text != "重启后仍可读" {
		t.Fatalf("重启后文本内容不符: %q", rr.Text)
	}
	if !strings.Contains(readFile(t, srv2.WsURL(), xlsxID).Text, "重启前写入") {
		t.Fatal("重启后看不到重启前的编辑结果")
	}
	if rp := call(t, srv2.WsURL(), "read", protocol.FileReadPayload{ID: "no-such-id"}); rp.OK {
		t.Fatal("重启后不存在的 id 仍被读到（目录或库张冠李戴）")
	}
}
