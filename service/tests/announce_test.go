// 设置回传（服务 → 中心）：服务侧的值自己变了时，AnnounceSetting 主动报个事实。
// 只报事实、不请求批准；秘密项只回「有没有值」，值永不上线；序号服务内单调递增。
package tests

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

// waitAnnounce 等到至少 n 条设置回传。
func waitAnnounce(t *testing.T, sc *stubCenter, n int, timeout time.Duration) []announceRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := sc.Announcements(); len(got) >= n {
			return got
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等待设置回传超时（期望 %d 条）", n)
	return nil
}

// TestAnnounceSetting 覆盖：普通项回传当前生效值、秘密项只回「有没有值」、序号单调、未声明键忽略。
func TestAnnounceSetting(t *testing.T) {
	sc := startStubCenter(t)
	logger := log.New(os.Stdout, "[svc-test] ", log.LstdFlags)

	rec, _ := newRecorder()
	s := service.New(service.Config{
		Name:              "svc-a",
		Listen:            "127.0.0.1:0",
		Center:            sc.URL(),
		HeartbeatInterval: 100 * time.Millisecond,
		SecretFile:        filepath.Join(t.TempDir(), "svc-a-secret.json"),
		Logger:            logger,
	})
	s.HandleSetting("demo.mode", "each", rec)
	s.HandleSecret("llm.key", func(string) error { return nil })

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	// 未声明的键：记一条日志就算了，不发出去（中心那边不认）
	s.AnnounceSetting("ghost")
	time.Sleep(100 * time.Millisecond)
	if got := sc.Announcements(); len(got) != 0 {
		t.Fatalf("未声明的键不该回传: %+v", got)
	}

	// 普通项：回传的是当前生效值（下发过一次，所以是下发后的值）
	if rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "demo.mode", Value: "batch"}); !rp.OK {
		t.Fatalf("configure 应成功: %+v", rp)
	}
	s.AnnounceSetting("demo.mode")
	got := waitAnnounce(t, sc, 1, 5*time.Second)
	if got[0].From != "svc-a" || got[0].P.Service != "svc-a" || got[0].P.Key != "demo.mode" || got[0].P.Value != "batch" {
		t.Fatalf("普通项回传不符: %+v", got[0])
	}
	if got[0].P.Seq == 0 {
		t.Fatalf("回传应带单调序号: %+v", got[0])
	}

	// 秘密项：只回「有没有值」，值永不上线
	if rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "llm.key", Value: "sk-announce"}); !rp.OK {
		t.Fatalf("秘密下发应成功: %+v", rp)
	}
	s.AnnounceSetting("llm.key")
	got = waitAnnounce(t, sc, 2, 5*time.Second)
	last := got[len(got)-1]
	if last.P.Key != "llm.key" || !last.P.Set || last.P.Value != "" {
		t.Fatalf("秘密项回传不该带值（只回有没有值）: %+v", last.P)
	}
	if last.P.Seq <= got[0].P.Seq {
		t.Fatalf("序号应单调递增: %d → %d", got[0].P.Seq, last.P.Seq)
	}
}
