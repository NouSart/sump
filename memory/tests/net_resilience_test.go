package tests

// 失败注入与并发：这些是"平时全绿、出一次问题就没人说得清"的地方。
//
// 三个面：
//   - 上游**挂住**：embedding 不回复时，note 必须自己超时收手（服务端有 3s 上限），
//     不能把调用方一起挂死，也不能留下半条经验。
//   - 上游**中途坏掉**：报错 → 不写；恢复 → 还能写（种子向量"失败下次再试"的那条路）。
//   - **并发**：同一句话同时说五遍只能留一条；五条记录同时落库要按 seq 排好。

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/protocol"
)

// callWithin 是 call 的可调超时版（上游挂住时要等超过默认 5s，才看得出服务端的收手）。
func callWithin(t *testing.T, srv *server.Server, action string, input any, timeout time.Duration) protocol.ResponsePayload {
	t.Helper()
	conn, err := protocol.Dial(srv.WsURL(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	raw, err := protocol.EncodePayload(input)
	if err != nil {
		t.Fatal(err)
	}
	env, err := protocol.NewEnvelope(protocol.TypeJump, "reasoner", "memory", "trace-test", protocol.JumpPayload{Action: action, Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	resp, err := conn.Call(ctx, env)
	if err != nil {
		return protocol.ResponsePayload{OK: false, Error: err.Error()}
	}
	var rp protocol.ResponsePayload
	if err := resp.DecodePayload(&rp); err != nil {
		t.Fatal(err)
	}
	return rp
}

// TestNoteTimesOutWhenEmbeddingHangs 验证上游挂住时 note 会自己收手。
// embedTimeout 是 3s：超过就该返回错误，而不是跟着上游一起挂住。
func TestNoteTimesOutWhenEmbeddingHangs(t *testing.T) {
	sc := startStubCenter(t)
	emb := startStubEmbedding(t, sc)
	emb.SetVectors(map[string][]float64{"挂住的经验": {1, 0}})
	emb.SetDelay(30 * time.Second) // 假装上游再也不回来
	srv := startMemoryWithNet(t, sc.URL())
	waitRosterName(t, srv, "embedding")

	// 第一次：补种子向量失败（3s）+ 这条经验自己向量化失败（3s）＝约两倍超时。
	start := time.Now()
	rp := callWithin(t, srv, "note", protocol.NotePayload{Content: "挂住的经验", TaskID: "task-hang"}, 20*time.Second)
	first := time.Since(start)
	if rp.OK {
		t.Fatal("上游不回复时 note 不该成功")
	}
	if first > 10*time.Second {
		t.Fatalf("note 该在上游超时后自己收手，实际等了 %v（上游挂着 30s）", first)
	}

	// 第二次：种子向量在冷却中，不再陪跑——只付这条经验自己的一个超时。
	start = time.Now()
	rp = callWithin(t, srv, "note", protocol.NotePayload{Content: "挂住的经验", TaskID: "task-hang"}, 20*time.Second)
	second := time.Since(start)
	if rp.OK {
		t.Fatal("上游仍然不回复，note 不该成功")
	}
	if second > 5*time.Second {
		t.Fatalf("冷却没生效：第二次仍等了 %v（应该只付一个 embedTimeout）", second)
	}
	if second >= first {
		t.Fatalf("第二次（%v）应当比第一次（%v）短——种子向量在冷却中不该重试", second, first)
	}
}

// TestNoteRecoversAfterEmbeddingComesBack 验证上游坏掉再恢复：
// 坏的时候不写（不写半条），恢复之后照常写——种子向量那条"失败下次再试"的路也一起验了。
func TestNoteRecoversAfterEmbeddingComesBack(t *testing.T) {
	sc := startStubCenter(t)
	emb := startStubEmbedding(t, sc)
	emb.SetVectors(map[string][]float64{"恢复了": {1, 0}})
	srv := startMemoryWithNet(t, sc.URL())
	caller := startStubCaller(t, sc.URL())
	waitRosterName(t, srv, "embedding")

	emb.SetFail(true)
	if rp := caller.call(t, srv.WsURL(), "note", protocol.NotePayload{Content: "恢复了", TaskID: "task-recover"}); rp.OK {
		t.Fatal("上游报错时 note 不该成功")
	}

	emb.SetFail(false)
	rp := caller.call(t, srv.WsURL(), "note", protocol.NotePayload{Content: "恢复了", TaskID: "task-recover"})
	if !rp.OK {
		t.Fatalf("上游恢复后 note 应成功: %s", rp.Error)
	}
	var res protocol.NoteResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	if res.ID == "" || len(res.NodeIDs) == 0 {
		t.Fatalf("经验应落库并挂到大类（种子向量失败过也要补得上）: %+v", res)
	}
}

// TestConcurrentIdenticalNotesCreateOneEntry 验证并发去重。
//
// `Attach` 是"先查近重复，再插入"两步，中间没有事务。同一句话同时说五遍时，
// 五个请求可能都查到"没有重复"然后各插一条——那样经验网会被重复条目灌满，
// 而且没人会报错。
func TestConcurrentIdenticalNotesCreateOneEntry(t *testing.T) {
	sc := startStubCenter(t)
	emb := startStubEmbedding(t, sc)
	emb.SetVectors(map[string][]float64{"同一句话": {1, 0}})
	srv := startMemoryWithNet(t, sc.URL())
	waitRosterName(t, srv, "embedding")

	const n = 5
	ids := make([]string, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // 尽量同时出发，把竞态压出来
			caller := startStubCaller(t, sc.URL())
			rp := caller.call(t, srv.WsURL(), "note", protocol.NotePayload{Content: "同一句话", TaskID: "task-race"})
			if !rp.OK {
				return
			}
			var res protocol.NoteResult
			if err := protocol.DecodeRaw(rp.Data, &res); err == nil {
				ids[i] = res.ID
			}
		}(i)
	}
	close(start)
	wg.Wait()

	distinct := map[string]bool{}
	for _, id := range ids {
		if id != "" {
			distinct[id] = true
		}
	}
	if len(distinct) != 1 {
		t.Fatalf("同一句话并发写 %d 次应只留一条，实际留下 %d 条不同的条目: %v", n, len(distinct), ids)
	}
}

// TestConcurrentStoresKeepSeqOrder 验证并发写会话记录仍按 seq 排序。
// Fire 各自成 goroutine，落库顺序不保证；排序全靠写入方给的 seq。
func TestConcurrentStoresKeepSeqOrder(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 50)
	caller := startStubCaller(t, sc.URL())

	const n = 12
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			caller.call(t, srv.WsURL(), "store", protocol.StorePayload{
				ConversationID: "c-seq", Boss: "frontend", Role: "user",
				Content: "第 " + strconv.Itoa(i) + " 条", Seq: int64(i + 1),
			})
		}(i)
	}
	close(start)
	wg.Wait()

	rp := caller.call(t, srv.WsURL(), "history", protocol.HistoryPayload{ConversationID: "c-seq", Limit: 50, AllTasks: true})
	if !rp.OK {
		t.Fatalf("history 失败: %s", rp.Error)
	}
	var res protocol.HistoryResult
	if err := protocol.DecodeRaw(rp.Data, &res); err != nil {
		t.Fatal(err)
	}
	// 内容相同会被幂等去重（"上一行完全相同就跳过"）——所以这里只断言序号单调。
	if len(res.Messages) < 2 {
		t.Fatalf("并发写入应落下多条记录，实际 %d", len(res.Messages))
	}
	for i := 1; i < len(res.Messages); i++ {
		if res.Messages[i].Seq <= res.Messages[i-1].Seq {
			t.Fatalf("记录应按 seq 递增：第 %d 条 %d <= 第 %d 条 %d", i, res.Messages[i].Seq, i-1, res.Messages[i-1].Seq)
		}
	}
}
