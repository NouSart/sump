package tests

// 交付失败的处理（真进程）：第一次 deliver 注入错误，reasoner 必须停住而不是假装成功；
// 同一会话的后续消息仍能重新走到交付——失败不能把任务卡死。

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestDeliverFailureKeepsTaskRetryableE2E(t *testing.T) {
	scBin := buildService(t, "github.com/NouSart/sump/settings-center")
	memBin := buildService(t, "github.com/NouSart/sump/memory")
	alBin := buildService(t, "github.com/NouSart/sump/reasoner")
	fakeLLM := startFakeDeepSeek(t, false)
	boss := startE2EBoss(t)
	boss.failNext(1)

	scPort := freePort(t)
	memPort := freePort(t)
	alPort := freePort(t)
	centerURL := spv2URL(scPort)
	memoryURL := spv2URL(memPort)
	reasonerURL := spv2URL(alPort)

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)
	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(t.TempDir(), "memory.db"),
	)
	waitWSReady(t, memoryURL, 60*time.Second)
	bossClient := boss.register(t, centerURL)
	t.Cleanup(func() { _ = bossClient.Close() })
	startProc(t, alBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", alPort),
		"-center", centerURL,
		"-llm-base", fakeLLM.URL(),
		"-llm-key", "test-key",
		"-llm-model", "fake-model",
		"-reflect=false",
	)
	waitWSReady(t, reasonerURL, 60*time.Second)

	obs := dialSPV2(t, centerURL)
	waitRosterHas(t, obs, "reasoner", 60*time.Second)
	waitRosterHas(t, obs, "memory", 10*time.Second)

	rc := dialSPV2(t, reasonerURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const conv = "e2e-deliver-fail"

	ack := callJumpFrom(t, rc, "e2e-boss", "reasoner", "user_message", map[string]any{"text": "第一次", "conversation_id": conv}, ctx)
	if !ack.OK {
		t.Fatalf("user_message 未被受理: %s", ack.Error)
	}

	// 第一次交付确实发生了（并被注入失败），不能计入成功。
	select {
	case got := <-boss.attempts:
		if got == "" {
			t.Fatal("交付尝试的文本为空")
		}
	case <-time.After(30 * time.Second):
		t.Fatal("超时：reasoner 没有尝试交付")
	}
	select {
	case text := <-boss.delivered:
		t.Fatalf("注入了交付失败，却记录了成功交付: %q", text)
	case <-time.After(time.Second):
	}

	// 失败之后同一会话继续说话：仍应被受理并最终交付。
	ack2 := callJumpFrom(t, rc, "e2e-boss", "reasoner", "user_message", map[string]any{"text": "第二次", "conversation_id": conv}, ctx)
	if !ack2.OK {
		t.Fatalf("失败后的第二条 user_message 未被受理: %s", ack2.Error)
	}
	select {
	case text := <-boss.delivered:
		if text != "E2E 最终答复" {
			t.Fatalf("交付文本不符: %q", text)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("超时：交付失败后同一会话的后续消息没有交付")
	}
}
