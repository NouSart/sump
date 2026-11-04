package tests

// 任务存储：一个会话最多一个进行中任务；EnsureTask 并发安全；完结/恢复的状态流转。

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/NouSart/sump/memory/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "tasks.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func TestEnsureTaskIsSingleton(t *testing.T) {
	st := openStore(t)

	first, err := st.EnsureTask("c", "第一个任务")
	if err != nil {
		t.Fatal(err)
	}
	again, err := st.EnsureTask("c", "不该被用到")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != first.ID {
		t.Fatalf("已有进行中任务时不该新建: %s vs %s", first.ID, again.ID)
	}

	// 并发抢建：唯一索引保证只有一个能建上，其余回头读同一个。
	var wg sync.WaitGroup
	ids := make([]string, 8)
	for i := range ids {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task, err := st.EnsureTask("race", "并发任务")
			if err != nil {
				t.Errorf("EnsureTask 并发失败: %v", err)
				return
			}
			ids[i] = task.ID
		}(i)
	}
	wg.Wait()
	for _, id := range ids {
		if id != ids[0] {
			t.Fatalf("并发下任务分叉了: %v", ids)
		}
	}
}

func TestTaskLifecycle(t *testing.T) {
	st := openStore(t)

	task, err := st.EnsureTask("c", "把这事做完")
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != store.TaskActive {
		t.Fatalf("新任务应是进行中: %+v", task)
	}

	// 记一条消息 → 归属这个任务，并刷新它的活跃时间。
	if err := st.Record("c", "user", "开始", "", "", 1, task.ID); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchTask(task.ID); err != nil {
		t.Fatal(err)
	}
	hist, err := st.History("c", 0, 0, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].TaskID != task.ID {
		t.Fatalf("消息应归属任务: %+v", hist)
	}

	// 改名 + 完结
	if _, err := st.RenameTask(task.ID, "改好的名字"); err != nil {
		t.Fatal(err)
	}
	done, err := st.FinishTask(task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if done.Status != store.TaskDone || done.Name != "改好的名字" {
		t.Fatalf("完结后状态/名字不符: %+v", done)
	}

	// 完结后可以再开一个新任务（同会话允许有多个任务，但只有一个 active）
	next, err := st.EnsureTask("c", "下一件事")
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == task.ID {
		t.Fatal("完结后应开新任务")
	}
	list, err := st.ListTasks("c", 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].ID != next.ID {
		t.Fatalf("任务列表应按最近活跃排序: %+v", list)
	}
}

// TestTaskScopedHistory 验证历史按任务过滤：另一个任务的消息不会串进来。
func TestTaskScopedHistory(t *testing.T) {
	st := openStore(t)

	a, err := st.EnsureTask("c", "任务 A")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Record("c", "user", "A 的消息", "", "", 1, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.FinishTask(a.ID, ""); err != nil {
		t.Fatal(err)
	}
	b, err := st.EnsureTask("c", "任务 B")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Record("c", "user", "B 的消息", "", "", 2, b.ID); err != nil {
		t.Fatal(err)
	}

	histA, err := st.History("c", 0, 0, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	histB, err := st.History("c", 0, 0, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(histA) != 1 || histA[0].Content != "A 的消息" {
		t.Fatalf("任务 A 的历史不对: %+v", histA)
	}
	if len(histB) != 1 || histB[0].Content != "B 的消息" {
		t.Fatalf("任务 B 的历史不对: %+v", histB)
	}

	// LLM 上下文也按任务取：任务 B 只看到自己那条。
	recent, err := st.Recent("c", 10, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Content != "B 的消息" {
		t.Fatalf("Recent 应按任务过滤: %+v", recent)
	}
}
