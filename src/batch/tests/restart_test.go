package tests

import (
	"context"
	"log"
	"testing"
	"time"

	"github.com/NouSart/sump/batch/server"
	"github.com/NouSart/sump/protocol"
)

// TestRestartTurnsRunningIntoPaused：服务重启后，没跑完的任务转 paused，
// 重启时正在跑的项标失败（结果未知），resume 后已完成的项不重跑。
func TestRestartTurnsRunningIntoPaused(t *testing.T) {
	sc := startStubCenter(t)
	tg := startTarget(t, sc)
	dbPath := t.TempDir() + "/batch.db"

	startBatch := func() *server.Server {
		b := server.New(server.Config{
			Name: "batch", Listen: "127.0.0.1:0", Center: sc.URL(), DBPath: dbPath,
		}, log.New(testWriter{t}, "[batch] ", 0))
		if err := b.Start(context.Background()); err != nil {
			t.Fatal(err)
		}
		return b
	}

	b1 := startBatch()
	reports := make(chan protocol.AsyncResultPayload, 64)
	probe := startCaller(t, sc, "probe", b1.WsURL(), reports)
	f := &fixture{batch: b1, target: tg, center: sc, probe: probe, reports: reports}
	list := make([]protocol.BatchItem, 4)
	for i := range list {
		list[i] = protocol.BatchItem{Input: mustRaw(map[string]any{"ms": 3000})}
	}
	res := f.mustRun(t, protocol.BatchRunPayload{Service: "worker", Action: "slow", Items: list, Concurrency: 2})
	time.Sleep(200 * time.Millisecond) // 让两项真的跑起来

	// 模拟进程死掉：不走 Shutdown，直接丢掉实例（DB 里 phase 仍是 running）。
	b1.Shutdown()
	time.Sleep(100 * time.Millisecond)

	b2 := startBatch()
	t.Cleanup(b2.Shutdown)
	f.batch = b2

	// 重启后 batch 换了一个监听地址：旧调用方的连接随之作废，换一条连新实例的。
	probe.close()
	probe2 := startCaller(t, sc, "probe2", b2.WsURL(), nil)
	f.probe = probe2

	got := f.status(t, res.ItemID)
	if got.Phase != protocol.BatchPhasePaused {
		t.Fatalf("重启后应转 paused，实际 %s", got.Phase)
	}
	if got.Failed == 0 {
		t.Fatalf("重启时正在跑的项应标失败（结果未知），实际 %+v", got)
	}

	// 恢复：只剩未开始的项会跑。重启中断的那两项已标失败，所以终态是 failed。
	f.control(t, protocol.BatchControlPayload{Op: protocol.BatchOpResume, ItemID: res.ItemID})
	deadline := time.Now().Add(20 * time.Second)
	var done protocol.BatchStatusResult
	for time.Now().Before(deadline) {
		done = f.status(t, res.ItemID)
		if done.Phase == protocol.BatchPhaseFailed || done.Phase == protocol.BatchPhaseDone {
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if done.Phase != protocol.BatchPhaseFailed {
		t.Fatalf("有中断项时终态应为 failed，实际 %+v", done)
	}
	if done.Ok+done.Failed+done.Skipped != 4 {
		t.Fatalf("跑完 + 失败 + 跳过应等于 4，实际 %+v", done)
	}
	if done.Ok < 2 {
		t.Fatalf("恢复后剩余项应跑完，实际 %+v", done)
	}
}
