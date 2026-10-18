// 经验网的真进程端到端：note → 召回 → resume 里的记忆块。
//
// 这条用例存在的理由：memnet 的包内单测喂的是**手写向量**，只证明数据层算术对；
// 它证明不了"一句经验真能走到模型眼前"。这里跑真进程（center + memory + Python
// embedding fake 后端），验的是**数据流转**：
//
//	note（带向量化） → 落库并挂到大类 → recall 组装记忆块 → resume 里出现「经验线头」
//
// fake 后端是确定性 bigram 哈希（无语义，但排序可预期），所以用例只断言
// "相关的那条进了线头、无关的不进"，不涉及阈值调优。
package tests

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/protocol"
)

func TestMemnetDataFlowE2E(t *testing.T) {
	scBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/settings-center")
	memBin := buildService(t, "github.com/QAQ-awa-QAQ/sump/memory")

	scPort := freePort(t)
	memPort := freePort(t)
	centerURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", scPort)
	memURL := fmt.Sprintf("ws://127.0.0.1:%d/ws", memPort)
	dir := t.TempDir()

	startProc(t, scBin, "-addr", fmt.Sprintf("127.0.0.1:%d", scPort))
	waitWSReady(t, centerURL, 60*time.Second)

	startProc(t, memBin,
		"-addr", fmt.Sprintf("127.0.0.1:%d", memPort),
		"-center", centerURL,
		"-db", filepath.Join(dir, "memory.db"),
	)
	waitWSReady(t, memURL, 60*time.Second)

	obs, _ := registerObserver(t, centerURL)
	waitRosterHas(t, obs, "memory", 60*time.Second)

	// embedding 后上线：顺带验"种子向量懒补齐"（note 之前网里还没有大类向量）。
	embPort := freePort(t)
	startEmbeddingProc(t, centerURL, embPort)
	waitWSReady(t, fmt.Sprintf("ws://127.0.0.1:%d/ws", embPort), 60*time.Second)
	waitRosterHas(t, obs, "embedding", 60*time.Second)

	probe := startResumeProbe(t, centerURL)

	const (
		relevant  = "进程内计数器做消息 seq，重启之后序号会倒流"
		unrelated = "用户喜欢整站配色跟随本地时间变化"
	)
	for _, content := range []string{relevant, unrelated} {
		rp := memoryCall(t, memURL, "note", protocol.NotePayload{
			Content: content,
			TaskID:  "task-e2e-net",
		})
		if !rp.OK {
			t.Fatalf("note %q 失败: %s", content, rp.Error)
		}
		var res protocol.NoteResult
		if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
			t.Fatal(err)
		}
		if res.ID == "" || len(res.NodeIDs) == 0 {
			t.Fatalf("经验应落库并挂到大类: %+v", res)
		}
	}

	// 一次 recall：记忆块里必须出现「经验线头」，且只带相关的那条。
	block := recallEventually(t, probe, memURL, "消息顺序 进程内计数器 seq 重启 倒流", "e2e-memnet", func(b string) bool {
		return strings.Contains(b, "（经验线头）")
	})
	if !strings.Contains(block, "进程内计数器") {
		t.Fatalf("线头里应出现相关经验，实际记忆块:\n%s", block)
	}
	if strings.Contains(block, "整站配色") {
		t.Fatalf("无关经验不该被当成线头塞进上下文，实际记忆块:\n%s", block)
	}
	if !strings.Contains(block, "task-e2e-net") {
		t.Fatalf("线头应带上它来自哪个任务（可回溯），实际记忆块:\n%s", block)
	}

	// dig：顺着线头往下扒，entry_id 取自线头给出的成员 id（模拟模型"抓线头"）。
	var heads protocol.LineHeadsResult
	rp := memoryCall(t, memURL, "lineheads", protocol.LineHeadsPayload{
		Query: "消息顺序 进程内计数器 seq", Limit: 4,
	})
	if !rp.OK {
		t.Fatalf("lineheads 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &heads); err != nil {
		t.Fatal(err)
	}
	if len(heads.Heads) == 0 || len(heads.Heads[0].Members) == 0 {
		t.Fatalf("lineheads 应给出可下探的成员 id: %+v", heads)
	}
	var dig protocol.DigResult
	rp = memoryCall(t, memURL, "dig", protocol.DigPayload{EntryID: heads.Heads[0].Members[0], Limit: 5})
	if !rp.OK {
		t.Fatalf("dig 失败: %s", rp.Error)
	}
	if err := protocol.DecodeRaw(rp.Data, &dig); err != nil {
		t.Fatal(err)
	}
	if dig.EntryID == "" || dig.Content == "" {
		t.Fatalf("dig 应带回中心条目: %+v", dig)
	}

	// prune：软剪枝不该误伤（刚写的经验还新鲜）。
	rp = memoryCall(t, memURL, "prune", map[string]any{})
	if !rp.OK {
		t.Fatalf("prune 失败: %s", rp.Error)
	}
	var pr protocol.PruneResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Entries != 0 {
		t.Fatalf("新鲜经验不该被剪枝，实际 %d 条", pr.Entries)
	}
}

// recallEventually 反复 recall 直到记忆块满足条件（embedding/种子补齐是异步的）。
func recallEventually(t *testing.T, p *resumeProbe, memURL, query, conv string, ok func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	var last string
	var lastErr error
	for time.Now().Before(deadline) {
		block, err := recallOnce(t, p, memURL, query, conv)
		if err != nil {
			lastErr = err
			time.Sleep(200 * time.Millisecond)
			continue
		}
		last = block
		if ok(block) {
			return block
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("等待记忆块满足条件超时；最后一次:\n%s\n错误: %v", last, lastErr)
	return ""
}
