package server

// 控制面账本（受理记录）的白盒测试：状态推进、过期终态、条数上限。
// 为什么单独测这张表：它是 owner_status 的唯一事实来源，过期 / 上限的边界不在
// 端到端用例里（跑 1024 条命令太慢），只能在这里钉住。

import (
	"context"
	"fmt"
	"io"
	"log"
	"testing"
	"time"

	"github.com/NouSart/sump/protocol"
)

// newTrackedServer 建一个只用于账本测试的 shell 实例（不 Start，不跑命令）。
func newTrackedServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{Name: "shell", Root: t.TempDir()}, log.New(io.Discard, "", 0))
}

// TestTrackedLedger：受理 → 在跑 → 终态的推进能被 status 读到；过期终态不再被认识。
func TestTrackedLedger(t *testing.T) {
	s := newTrackedServer(t)
	ctx := context.Background()

	s.track("a", protocol.OwnerExecCreated)
	if exec, ok := s.trackedExec("a", time.Now()); !ok || exec != protocol.OwnerExecCreated {
		t.Fatalf("受理后应为 created: exec=%q ok=%v", exec, ok)
	}
	s.track("a", protocol.OwnerExecRunning)
	if exec, _ := s.trackedExec("a", time.Now()); exec != protocol.OwnerExecRunning {
		t.Fatalf("开跑后应为 running，实际 %q", exec)
	}
	if states, complete, err := s.OwnerStatus(ctx, []string{"a", "nope"}); err != nil {
		t.Fatal(err)
	} else if !complete || states["a"] != protocol.OwnerExecRunning || len(states) != 1 {
		t.Fatalf("status 不符: states=%v complete=%v", states, complete)
	}

	// 终态过期：把 updated 拨回窗口之外（等价于十/十一分钟后再问）。
	s.track("a", protocol.OwnerExecDone)
	s.trackMu.Lock()
	s.tracked["a"] = trackedJob{exec: protocol.OwnerExecDone, updated: time.Now().Add(-ownerTrackRetention - time.Second)}
	s.trackMu.Unlock()
	if _, ok := s.trackedExec("a", time.Now()); ok {
		t.Fatal("过期的终态不该再被认识")
	}
	s.track("b", protocol.OwnerExecCreated) // 任何一次记账都会顺带清理
	s.trackMu.Lock()
	_, exists := s.tracked["a"]
	s.trackMu.Unlock()
	if exists {
		t.Fatal("过期的终态应被清理掉")
	}
}

// TestTrackedCap：条数超上限时只挤最老的终态；在跑的一条都不动。
func TestTrackedCap(t *testing.T) {
	s := newTrackedServer(t)
	s.track("live", protocol.OwnerExecRunning)
	const extra = 50
	for i := 0; i < ownerTrackMax+extra; i++ {
		s.track(fmt.Sprintf("j-%05d", i), protocol.OwnerExecDone)
	}

	s.trackMu.Lock()
	size := len(s.tracked)
	_, liveOK := s.tracked["live"]
	_, oldest := s.tracked["j-00000"]
	_, newest := s.tracked[fmt.Sprintf("j-%05d", ownerTrackMax+extra-1)]
	s.trackMu.Unlock()

	if size > ownerTrackMax {
		t.Fatalf("账本条数应被压回上限 %d，实际 %d", ownerTrackMax, size)
	}
	if !liveOK {
		t.Fatal("在跑的任务不该被挤掉")
	}
	if oldest {
		t.Fatal("最老的终态应先被挤掉")
	}
	if !newest {
		t.Fatal("最新的终态应保留")
	}
}
