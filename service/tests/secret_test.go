// 骨架侧三类设置项：秘密项（本服务持有 + 落盘 + 重启带回 + 空值清除）、
// 只读项（明确拒绝）、名片声明（秘密只报「有没有值」，只读带启动值）。
package tests

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/service"
)

// TestSecretAndReadOnly 覆盖秘密项与只读项的完整生命周期。
func TestSecretAndReadOnly(t *testing.T) {
	sc := startStubCenter(t)
	logger := log.New(os.Stdout, "[svc-test] ", log.LstdFlags)
	secretFile := filepath.Join(t.TempDir(), "svc-s-secret.json")

	// newSvc 起一个声明了「普通 + 秘密 + 只读」各一项的服务实例（秘密回调由调用方给）。
	newSvc := func(rec func(string) error) *service.Service {
		s := service.New(service.Config{
			Name:              "svc-s",
			Listen:            "127.0.0.1:0",
			Center:            sc.URL(),
			HeartbeatInterval: 100 * time.Millisecond,
			SecretFile:        secretFile,
			Logger:            logger,
		})
		s.HandleSetting("llm.base", "https://api.example.com", func(string) error { return nil })
		s.HandleSecret("llm.key", rec)
		s.HandleReadOnly("addr", "127.0.0.1:9101")
		return s
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	recKey, gotKey := newRecorder()
	s := newSvc(recKey)
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Shutdown)

	// 名片：三类声明各就各位——秘密项只有「有没有值」这一件事
	regs := sc.Registers()
	if len(regs) == 0 {
		t.Fatal("未收到注册")
	}
	decl := map[string]protocol.Setting{}
	for _, it := range regs[0].Settings {
		decl[it.Key] = it
	}
	if it, ok := decl["llm.key"]; !ok || !it.Secret || it.Set || it.Default != "" {
		t.Fatalf("秘密项声明不符（只该有 secret 标记）: %+v", it)
	}
	if it, ok := decl["addr"]; !ok || !it.ReadOnly || it.Default != "127.0.0.1:9101" {
		t.Fatalf("只读项声明不符（default 应为启动值）: %+v", it)
	}
	if it, ok := decl["llm.base"]; !ok || it.Secret || it.ReadOnly || it.Default != "https://api.example.com" {
		t.Fatalf("普通项声明不符: %+v", it)
	}

	// 秘密下发：回调拿到值、服务侧读得到、落盘（中心那边没有值，服务重启只能靠自己）
	if rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "llm.key", Value: "sk-1"}); !rp.OK {
		t.Fatalf("秘密下发应成功: %+v", rp)
	}
	if v := s.SettingValue("llm.key"); v != "sk-1" {
		t.Fatalf("服务侧应持有秘密，实际 %q", v)
	}
	if got := gotKey(); len(got) != 1 || got[0] != "sk-1" {
		t.Fatalf("秘密回调不符: %+v", got)
	}
	raw, err := os.ReadFile(secretFile)
	if err != nil || !strings.Contains(string(raw), "sk-1") {
		t.Fatalf("秘密未落盘: %v / %s", err, raw)
	}

	// 只读项：明确拒绝，生效值不动
	rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "addr", Value: "0.0.0.0:1"})
	if rp.OK || !strings.Contains(rp.Error, "只读") {
		t.Fatalf("只读项应被拒绝: %+v", rp)
	}
	if v := s.SettingValue("addr"); v != "127.0.0.1:9101" {
		t.Fatalf("只读值不该变，实际 %q", v)
	}

	// 空值 = 清除（面板的「清空」走这条）：文件里也不该再留着
	if rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "llm.key", Value: ""}); !rp.OK {
		t.Fatalf("清空秘密应成功: %+v", rp)
	}
	if v := s.SettingValue("llm.key"); v != "" {
		t.Fatalf("清空后生效值应为空，实际 %q", v)
	}
	if raw, err := os.ReadFile(secretFile); err == nil && strings.Contains(string(raw), "sk-1") {
		t.Fatalf("清空后磁盘上还留着值: %s", raw)
	}

	// 再设置一次，然后重启（同一个秘密文件）：Start 时把值带起来并应用
	if rp := pushConfigure(t, s.WsURL(), protocol.ConfigurePayload{Key: "llm.key", Value: "sk-2"}); !rp.OK {
		t.Fatalf("秘密下发应成功: %+v", rp)
	}
	s.Shutdown()

	recKey2, gotKey2 := newRecorder()
	s2 := newSvc(recKey2)
	if err := s2.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s2.Shutdown)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(gotKey2()) == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if got := gotKey2(); len(got) == 0 || got[0] != "sk-2" {
		t.Fatalf("重启后应加载本地秘密并应用，实际 %+v", got)
	}
	if v := s2.SettingValue("llm.key"); v != "sk-2" {
		t.Fatalf("重启后生效值应为 sk-2，实际 %q", v)
	}

	// 重启后注册的名片：只有「有值」，值本身不上线
	regs = sc.Registers()
	if len(regs) < 2 {
		t.Fatalf("第二次注册没到: %d 条", len(regs))
	}
	for _, it := range regs[len(regs)-1].Settings {
		if it.Key != "llm.key" {
			continue
		}
		if !it.Secret || !it.Set || it.Default != "" {
			t.Fatalf("重连名片仍不该带值: %+v", it)
		}
	}
}
