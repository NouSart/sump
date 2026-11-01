// 跨会话共享经验库的真进程端到端：两个 conversation_id 之间到底共享什么。
//
// 这条用例守的是**边界**，不是功能：
//
//	A 会话存原文 + 发布摘要 → B 会话 recall → 记忆块里有摘要，**没有** A 的原文、会话 id、命令；
//	两个库文件真的分开了（会话库里有 messages，共享库里没有）；
//	删掉共享物后 B 立刻看不到。
package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

func TestSharedExperienceE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	memBin := buildService(t, "github.com/NouSart/sump/memory")

	scPort := freePort(t)
	memPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	memURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
	dir := t.TempDir()
	privDB := filepath.Join(dir, "memory.db")
	sharedDB := filepath.Join(dir, "experience.db")

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)

	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", privDB,
	)
	waitWSReady(t, memURL, 60*time.Second)
	obs, _ := registerObserver(t, centerURL)
	waitRosterHas(t, obs, "memory", 60*time.Second)

	probe := startResumeProbe(t, centerURL)

	const (
		ownerCid = "qq:private:42"
		otherCid = "web:private:owner"
		secret   = "机密原文：数据库口令是 hunter2"
	)
	// 1) A 会话留一条原文（只该待在自己的会话库里）。
	rp := memoryCall(t, memURL, "store", protocol.StorePayload{
		ConversationID: ownerCid, Role: "user", Content: secret,
	})
	if !rp.OK {
		t.Fatalf("store 失败: %s", rp.Error)
	}
	// 2) A 发布摘要（复盘在真实链路里做的同一件事）。
	rp = memoryCall(t, memURL, "publish_session", protocol.PublishSessionPayload{
		ConversationID: ownerCid,
		TaskID:         "task-e2e-shared",
		Title:          "排查 embedding",
		Goal:           "排查 embedding 缺模型的报错",
		Outcome:        "已定位到模型文件缺失",
		Capabilities:   []string{"shell", "files"},
	})
	if !rp.OK {
		t.Fatalf("publish_session 失败: %s", rp.Error)
	}

	// 3) B 会话 recall：看到摘要，看不到原文。
	block := recallEventually(t, probe, memURL, "继续改门户", otherCid, func(b string) bool {
		return strings.Contains(b, "（其他会话）")
	})
	if !strings.Contains(block, "QQ「排查 embedding」") {
		t.Fatalf("目录里应有 A 的摘要:\n%s", block)
	}
	if !strings.Contains(block, "已定位到模型文件缺失") {
		t.Fatalf("应带上做到哪了:\n%s", block)
	}
	for _, banned := range []string{secret, ownerCid, "hunter2"} {
		if strings.Contains(block, banned) {
			t.Fatalf("泄漏了别的会话的东西 %q:\n%s", banned, block)
		}
	}

	// 4) 库文件是真的分开的：会话库有 messages，共享库里没有这张表。
	//
	// SQLite 把建表语句原文存在 sqlite_master 里，所以直接看文件字节就能判断
	// "这张表在不在"——不必给测试模块加一个 sqlite 驱动依赖。
	if !fileContains(t, privDB, "CREATE TABLE messages") {
		t.Fatal("会话库里应有 messages 表")
	}
	if !fileContains(t, sharedDB, "CREATE TABLE shared_tasks") {
		t.Fatal("共享库里应有会话目录表")
	}
	if fileContains(t, sharedDB, "CREATE TABLE messages") {
		t.Fatal("共享库里不该有 messages 表（原文只在会话库）")
	}
	if fileContains(t, sharedDB, "CREATE TABLE tasks") {
		t.Fatal("共享库里不该有 tasks 表")
	}

	// 5) 删掉共享物：B 立刻看不到（而不是等下次复盘）。
	rp = memoryCall(t, memURL, "shared_forget", protocol.SharedForgetPayload{SessionID: ownerCid})
	if !rp.OK {
		t.Fatalf("shared_forget 失败: %s", rp.Error)
	}
	var after protocol.SharedSessionsResult
	rp = memoryCall(t, memURL, "shared_sessions", protocol.SharedSessionsPayload{Exclude: otherCid})
	if !rp.OK {
		t.Fatalf("shared_sessions 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Sessions) != 0 {
		t.Fatalf("删完目录应为空: %+v", after.Sessions)
	}

	// 6) 原文并没有丢：A 自己 recall 时还在（只是别的会话读不到）。
	// 这条与上面一起构成"私有 / 共享"的完整对照——不然"看不到原文"可能只是因为它根本没存下来。
	own := recallEventually(t, probe, memURL, "继续", ownerCid, func(b string) bool {
		return strings.Contains(b, secret)
	})
	if strings.Contains(own, "（其他会话）") {
		t.Fatalf("自己的会话不该在「其他会话」一节里出现自己:\n%s", own)
	}
}

// fileContains 在文件字节里找一段文本（SQLite 的建表语句是明文存在 sqlite_master 里的）。
func fileContains(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读 %s 失败: %v", path, err)
	}
	return strings.Contains(string(data), needle)
}
