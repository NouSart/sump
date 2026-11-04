package tests

import (
	"io"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/qq/server"
)

// 失败提交不应断开已连接的网关，后续消息仍走原连接。
func TestFailedTokenSavePreservesConnection(t *testing.T) {
	sc := startStubCenter(t)
	nc := startStubNapCat(t)
	reasoner := startStubReasoner(t, sc)
	path := filepath.Join(t.TempDir(), "secret.json")
	s := server.New(server.Config{
		Name: "qq", Listen: "127.0.0.1:0", Center: sc.URL(),
		NapCatURL: nc.URL(), NapCatToken: "fixture-startup", Owner: "123",
		SecretFile: path,
	}, log.New(io.Discard, "", 0))
	if err := s.Start(t.Context()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)
	nc.waitConn(t, 5*time.Second)
	reasoner.sendDeliver(t, s.WsURL(), "before", "qq:private:123")
	nc.mu.Lock()
	before := nc.conn
	nc.mu.Unlock()
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	for _, candidate := range []string{"fixture-replacement", ""} {
		if result := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "napcat.token", Value: candidate}); result.OK {
			t.Fatal("save unexpectedly succeeded")
		}
		reasoner.sendDeliver(t, s.WsURL(), "after", "qq:private:123")
		nc.mu.Lock()
		same := nc.conn == before
		nc.mu.Unlock()
		if !same {
			t.Fatal("failed save replaced the active connection")
		}
	}
}
