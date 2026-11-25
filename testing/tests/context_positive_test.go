package tests

// context/2 正向链路的 3 进程验收（R-026 P2c）：
//
// 这里是 works.task 接线的回归证据：work 经由 facade 的 work_open（session.work_open）
// 真实登记，任务由 EnsureTask 解析后写进 works.task；随后 store 产生触发条目，
// context 才能按 works.task → tasks → conversation_id 取到任务并组装。
// 修 P4.5 之前 works.task 恒为空，首次组装会直接 not_found——所以本用例走通即证明接线生效，
// 不靠任何直接写库。

import (
	"testing"

	"github.com/NouSart/sump/protocol"
)

func TestContextPositiveWorksTaskWiring(t *testing.T) {
	memURL := startRedTeamV2Stack(t)

	const (
		work = "w-v2-pos"
		from = "reasoner-v2"
		conv = "conv-v2-pos"
	)

	// 1) 登记 work + 任务（work_open 的 EnsureTask → LinkWorkTask）。
	openBody, err := redTeamCall(t, memURL, work, from, "work_open", protocol.WorkOpenPayload{
		Work: work, ConversationID: conv, TaskName: "v2 正向任务", Boss: from,
	})
	if err != nil {
		t.Fatalf("work_open 失败: %v", err)
	}
	var opened protocol.WorkOpenResult
	if err := protocol.DecodeRaw(openBody, &opened); err != nil {
		t.Fatalf("work_open 回执无法解码: %v", err)
	}
	if opened.TaskID == "" {
		t.Fatal("work_open 应返回 task_id（works.task 接线的证据）")
	}

	// 2) 用 v2 store 写一条消息，作为这次触发。
	storeBody, err := redTeamCall(t, memURL, work, from, "store", protocol.StorePayload{
		ConversationID: conv, Work: work, TaskID: opened.TaskID, Role: "user",
		Content: "第一轮问题", ClientItemID: "ci-v2-pos",
	})
	if err != nil {
		t.Fatalf("v2 store 失败: %v", err)
	}
	var stored protocol.StoreResult
	if err := protocol.DecodeRaw(storeBody, &stored); err != nil {
		t.Fatalf("store 回执无法解码: %v", err)
	}
	if stored.ItemID == "" {
		t.Fatal("store 应返回 item_id（context 的 trigger.item 靠它）")
	}
	req := protocol.ContextRequest{
		Work: work, Trigger: protocol.Trigger{Kind: "user", Item: stored.ItemID},
	}

	// 3) 首次组装：能组装出任务投影，说明 works.task 已被 work_open 写入。
	body, err := redTeamCall(t, memURL, work, from, "context", req)
	if err != nil {
		t.Fatalf("首次 context 组装失败（works.task 未接线时这里是 not_found）: %v", err)
	}
	var first protocol.ContextResponse
	if err := protocol.DecodeRaw(body, &first); err != nil {
		t.Fatalf("context 回执无法解码: %v", err)
	}
	if first.TriggerState != "assembled" || first.SnapshotID == "" || first.AssembledAt == "" {
		t.Fatalf("首次组装回执不符: %+v", first)
	}
	if first.Task.ID != opened.TaskID || first.Task.ConversationID != conv || first.Task.Name != "v2 正向任务" {
		t.Fatalf("任务投影不符（works.task 没指向 work_open 建的任务）: %+v", first.Task)
	}
	if len(first.Messages) == 0 || first.Messages[0].Role != "system" {
		t.Fatalf("首次组装应带消息链（system 在第一条）: %+v", first.Messages)
	}

	// 4) 本轮完成：trigger_complete 幂等回 completed。
	for i := 0; i < 2; i++ {
		body, err = redTeamCall(t, memURL, work, from, "trigger_complete", protocol.TriggerCompletePayload{
			Work: work, Trigger: req.Trigger,
		})
		if err != nil {
			t.Fatalf("第 %d 次 trigger_complete 失败: %v", i+1, err)
		}
		var done protocol.TriggerCompleteResult
		if err := protocol.DecodeRaw(body, &done); err != nil {
			t.Fatalf("trigger_complete 回执无法解码: %v", err)
		}
		if !done.Completed {
			t.Fatalf("trigger_complete 应回 completed=true: %+v", done)
		}
	}

	// 5) completed 后再取：只补 ACK，不带消息链，快照 id 指回首次。
	body, err = redTeamCall(t, memURL, work, from, "context", req)
	if err != nil {
		t.Fatalf("completed 后取 context 失败: %v", err)
	}
	var ack protocol.ContextResponse
	if err := protocol.DecodeRaw(body, &ack); err != nil {
		t.Fatalf("completed 回执无法解码: %v", err)
	}
	if !ack.Completed || ack.TriggerState != "completed" || len(ack.Messages) != 0 {
		t.Fatalf("completed 只应补 ACK: %+v", ack)
	}
	if ack.SnapshotID != first.SnapshotID {
		t.Fatalf("ACK 应指回原快照: %q vs %q", ack.SnapshotID, first.SnapshotID)
	}
}
