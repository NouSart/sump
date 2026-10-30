package tests

// 经验网四个动作在**记忆服务层**的契约（真 server + 真 store + 假 embedding）。
//
// memnet 包自己的测试喂的是手写向量，只有数据层；根 e2e 走的是真 Python 进程，慢且
// 只覆盖 "写进去 → 翻出来" 一条路。这里补的是中间那层：参数校验、近重复、聚类、
// 下探、剪枝——用假 embedding 把几何钉死，断言才能确定。

import (
	"testing"
	"time"

	"github.com/NouSart/sump/memory/memnet"
	"github.com/NouSart/sump/memory/server"
	"github.com/NouSart/sump/memory/store"
	"github.com/NouSart/sump/protocol"
)

// startMemoryWithNet 起一个带经验网的记忆服务（关掉中心化，让假向量的余弦直白可算）。
func startMemoryWithNet(t *testing.T, centerURL string) *server.Server {
	t.Helper()
	return startMemory(t, centerURL, 12, func(cfg *server.Config, st *store.Store) {
		net, err := memnet.Open(st.DB(), time.Now())
		if err != nil {
			t.Fatal(err)
		}
		tune := memnet.DefaultTuning()
		tune.Center = false // 假向量是二维直角坐标，中心化只会让断言难算
		net.SetTuning(tune)
		cfg.Net = net
	})
}

// TestNetActionsServiceLevel 走一遍 note → 近重复 → lineheads → dig → prune，
// 外加三条参数校验（空内容 / 拼错的耐用度 / 经验网没开）。
func TestNetActionsServiceLevel(t *testing.T) {
	sc := startStubCenter(t)
	emb := startStubEmbedding(t, sc)
	// 只给这几句配向量：A 与 B 成 60°（余弦 0.5）——低于近重复阈值 0.62（不会被合并），
	// 又高于聚类阈值 0.30（应聚成一簇）；C 与 A 反向（余弦 -1 → 记 0 分），直接出局。
	// C 与 B 的余弦是 -0.5，也不会被当成 B 的近重复。
	emb.SetVectors(map[string][]float64{
		"进程内计数器做 seq，重启会倒流": {1, 0},
		"计数器的另一种说法":         {0.5, 0.866},
		"配色跟随本地时间":          {-1, 0},
	})
	srv := startMemoryWithNet(t, sc.URL())
	caller := startStubCaller(t, sc.URL())
	waitRosterName(t, srv, "embedding")

	note := func(content, durability string) protocol.ResponsePayload {
		return caller.call(t, srv.WsURL(), "note", protocol.NotePayload{
			Content: content, ConversationID: "c-net", TaskID: "task-net", Durability: durability,
		})
	}

	// 1) 正常写入：落库并挂到大类。
	rp := note("进程内计数器做 seq，重启会倒流", "")
	if !rp.OK {
		t.Fatalf("note 失败: %s", rp.Error)
	}
	var first protocol.NoteResult
	if err := protocol.DecodeRaw(rp.Data, &first); err != nil {
		t.Fatal(err)
	}
	if first.ID == "" || len(first.NodeIDs) == 0 {
		t.Fatalf("经验应落库并挂到大类: %+v", first)
	}

	// 2) 同一件事再说一遍：加命中，不新增条目。
	rp = note("进程内计数器做 seq，重启会倒流", "")
	if !rp.OK {
		t.Fatalf("重复 note 失败: %s", rp.Error)
	}
	var dup protocol.NoteResult
	if err := protocol.DecodeRaw(rp.Data, &dup); err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate || dup.ID != first.ID {
		t.Fatalf("完全相同的经验应合并到同一条: first=%+v dup=%+v", first, dup)
	}

	// 3) 相似但不算同一件事的另一句：留成独立条目，但应和上一条聚成同一簇。
	rp = note("计数器的另一种说法", "")
	if !rp.OK {
		t.Fatalf("note 失败: %s", rp.Error)
	}
	var similar protocol.NoteResult
	if err := protocol.DecodeRaw(rp.Data, &similar); err != nil {
		t.Fatal(err)
	}
	if similar.Duplicate || similar.ID == first.ID {
		t.Fatalf("相似但不相同的话不该被合并（否则这条用例测的就不是聚类了）: %+v", similar)
	}

	// 4) 一条反向的：不该被同一簇吸走。
	if rp = note("配色跟随本地时间", ""); !rp.OK {
		t.Fatalf("note 失败: %s", rp.Error)
	}

	// 5) 线头：查询与 A 同向 → A、B 聚成一簇，C 不进。
	rp = caller.call(t, srv.WsURL(), "lineheads", protocol.LineHeadsPayload{
		Query: "进程内计数器做 seq，重启会倒流", Limit: 5,
	})
	if !rp.OK {
		t.Fatalf("lineheads 失败: %s", rp.Error)
	}
	var heads protocol.LineHeadsResult
	if err := protocol.DecodeRaw(rp.Data, &heads); err != nil {
		t.Fatal(err)
	}
	if len(heads.Heads) == 0 {
		t.Fatal("应至少给出一条线头")
	}
	if heads.Heads[0].Count != 2 {
		t.Fatalf("同向的两条应聚成一簇（count=2），实际 %+v", heads.Heads)
	}
	if heads.Heads[0].TaskID != "task-net" {
		t.Fatalf("线头应带上来源任务: %+v", heads.Heads[0])
	}

	// 6) 下探：以 A 为中心翻兄弟，B 应在近邻里。
	rp = caller.call(t, srv.WsURL(), "dig", protocol.DigPayload{EntryID: first.ID, Limit: 5})
	if !rp.OK {
		t.Fatalf("dig 失败: %s", rp.Error)
	}
	var dig protocol.DigResult
	if err := protocol.DecodeRaw(rp.Data, &dig); err != nil {
		t.Fatal(err)
	}
	if dig.EntryID != first.ID {
		t.Fatalf("dig 应带回中心条目: %+v", dig)
	}
	foundSibling := false
	for _, n := range dig.Notes {
		if n.Content == "计数器的另一种说法" {
			foundSibling = true
		}
	}
	if !foundSibling {
		t.Fatalf("同簇的另一条应该在近邻里: %+v", dig.Notes)
	}

	// 7) 剪枝：刚写的经验还新鲜，一条都不该被剪。
	rp = caller.call(t, srv.WsURL(), "prune", map[string]any{})
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

	// 8) 参数校验：空内容、拼错的耐用度都要明确拒绝，不能静默降级。
	if rp := note("   ", ""); rp.OK {
		t.Fatal("空内容的经验应被拒绝")
	}
	rpBad := note("某条经验", "Core")
	if rpBad.OK {
		t.Fatal("耐用度拼错（Core）应被拒绝——静默降级会让本该不衰减的经验慢慢消失")
	}
}

// TestNetActionsDisabled 验证没开经验网时四个动作都明确报错，而不是空转或崩。
func TestNetActionsDisabled(t *testing.T) {
	sc := startStubCenter(t)
	srv := startMemory(t, sc.URL(), 12) // 不注入 Net
	caller := startStubCaller(t, sc.URL())

	for _, action := range []string{"note", "dig", "lineheads", "prune"} {
		rp := caller.call(t, srv.WsURL(), action, map[string]any{"content": "x", "query": "x"})
		if rp.OK {
			t.Fatalf("经验网没开时 %s 应报错", action)
		}
	}
}
