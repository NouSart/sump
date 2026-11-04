package server

// 收尾门禁的直接测试（tryClose 的四个分支）。
//
// 为什么必须在包内测：#46 记着，这几个判断（`stale` / `pending > 0` / `!judged` / `judgeAsks < 2`）
// 用跨包用例也能"抓到"，但靠的是超时兜底——失败要 112s~159s，CI 的超时下等于没有保护。
// 这些都是可直接构造的字段，断言级别验一遍才真的守得住。

import (
	"io"
	"log"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
	"github.com/NouSart/sump/reasoner/llm"
	"github.com/NouSart/sump/service"
)

// newCloseServer 造一个只够跑 tryClose 的实例：真的 service（Fire 不 panic），日志丢弃，
// boss 留空——交付路径会走 notifyDeliveryFailure 的空 boss 分支，不产生网络动作。
func newCloseServer(t *testing.T) *Server {
	t.Helper()
	logger := log.New(io.Discard, "", 0)
	svc := service.New(service.Config{Name: "reasoner", Logger: logger})
	t.Cleanup(svc.Shutdown)
	return &Server{Service: svc, logger: logger, cfg: Config{Name: "reasoner"}}
}

// newTask 造一个水位自洽的任务：gen == seen == 1。
func newTask(boss string) *taskState {
	return &taskState{cid: "c1", boss: boss, taskID: "task-1", messages: []llm.Message{{Role: "user", Content: "hi"}}, pending: map[string]pendingJob{}, gen: 1, seen: 1}
}

// TestTryCloseRefusedWhileToolsPending 还有工具在跑：拒绝收尾，并把本轮答复留在链上等结果。
// 「停车」语义：seen 推到 gen，结果入账时才唤醒——不然会反复烧 LLM 轮次。
func TestTryCloseRefusedWhileToolsPending(t *testing.T) {
	s := newCloseServer(t)
	st := newTask("")
	st.pending["call-1"] = pendingJob{name: "files__read"}

	s.tryClose(st, st.gen, "我先答一句")

	if st.closed {
		t.Fatal("还有工具在跑时不许收尾（会丢掉工具结果）")
	}
	if st.gen != 3 || st.seen != 3 {
		t.Fatalf("应把答复+提醒入账（gen += 2）并停车（seen = gen）：gen=%d seen=%d", st.gen, st.seen)
	}
	if len(st.messages) != 3 {
		t.Fatalf("应追加两条消息（本轮答复 + 拒绝收尾的提醒），实际 %d 条", len(st.messages))
	}
	if notice := st.messages[2].Content; !strings.Contains(notice, "files__read") {
		t.Fatalf("提醒里要点名还在跑的工具：%q", notice)
	}
}

// TestTryCloseRefusedWhenSnapshotStale 本轮快照过期（期间有新进展）：拒绝收尾，但**不停车**——
// 没有工具会来唤醒，必须靠下一轮把新进展消化掉。
func TestTryCloseRefusedWhenSnapshotStale(t *testing.T) {
	s := newCloseServer(t)
	st := newTask("")
	st.gen = 5 // 本轮开始时是 1，期间涨到 5

	s.tryClose(st, 1, "用旧上下文答的")

	if st.closed {
		t.Fatal("快照过期时不许收尾（会漏掉期间发生的事）")
	}
	if st.seen == st.gen {
		t.Fatal("过期但无在跑工具时不该停车：没有谁会来唤醒这一轮")
	}
	if st.gen != 7 {
		t.Fatalf("应把答复+提醒入账：gen=%d，want 7", st.gen)
	}
}

// TestTryCloseAsksForJudgement 没判定就催，最多两次；任务为空时**不设**这道门禁。
func TestTryCloseAsksForJudgement(t *testing.T) {
	t.Run("第一次催", func(t *testing.T) {
		s := newCloseServer(t)
		st := newTask("")

		s.tryClose(st, st.gen, "答完了")

		if st.closed {
			t.Fatal("没判定不许交付")
		}
		if st.judgeAsks != 1 {
			t.Fatalf("应记一次催促：%d", st.judgeAsks)
		}
		if !strings.Contains(st.messages[len(st.messages)-1].Content, "task_status") {
			t.Fatalf("提醒里要告诉模型怎么判定：%q", st.messages[len(st.messages)-1].Content)
		}
	})

	t.Run("第二次催", func(t *testing.T) {
		s := newCloseServer(t)
		st := newTask("")
		st.judgeAsks = 1

		s.tryClose(st, st.gen, "还是没判")

		if st.closed {
			t.Fatal("第二次仍要提醒（上限是两次，不是一次）")
		}
		if st.judgeAsks != 2 {
			t.Fatalf("应记到第二次：%d", st.judgeAsks)
		}
	})

	t.Run("没有任务就没有判定对象", func(t *testing.T) {
		s := newCloseServer(t)
		st := newTask("")
		st.taskID = "" // 记忆服务没给任务（拿不到就不该拦）
		before := len(st.messages)

		s.tryClose(st, st.gen, "答完了")

		// 交付会失败（本用例没有 boss），但**不该**留下"先判定"的提醒：
		// 没有任务就没有判定对象，用门禁拦它只会白烧一轮。
		if st.judgeAsks != 0 {
			t.Fatalf("没有任务时不该催判定：judgeAsks=%d", st.judgeAsks)
		}
		if len(st.messages) != before {
			t.Fatalf("没有任务时不该追加提醒，实际消息 %d → %d 条", before, len(st.messages))
		}
	})
}

// 让测试里能确认"没有任务 / 有任务"两种形状都合法（协议字段语义）。
var _ = protocol.TaskFinishPayload{}
