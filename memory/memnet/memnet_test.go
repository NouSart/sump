package memnet_test

// 经验网：预制网、多挂靠、近重复、遗忘曲线、软剪枝、线头聚簇、下探近邻。
//
// 这里全部喂**手写向量**，不碰 embedding 服务——网是纯数据层，
// 向量从哪来是上层的事。这样挂靠、聚簇、衰减都能确定性验证。
//
// 真模型（bge-small-zh）量的带宽记录在这里，免得下次又拍脑袋：
//   - 短句向量各向异性：原始余弦的"同义 / 无关"几乎重叠（间隔 +0.068）；
//     减掉语料均值后拉开到 +0.216 —— 所以比较前一律先中心化。
//   - 中心化后：同一件事 ≈ 0.43~0.87，无关 ≤ 0.21 —— 这才是可用的阈值带宽。

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/memory/memnet"
	_ "modernc.org/sqlite"
)

// vec 造一个二维单位向量：把"相关/不相关"写成角度，便于断言。
func vec(a float64) []float32 {
	return []float32{float32(math.Cos(a)), float32(math.Sin(a))}
}

// openNet 让这个包自己管库：经验网只吃 *sql.DB，不关心它属于谁。
func openNet(t *testing.T) *memnet.Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "notes.db"))
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	st, err := memnet.Open(db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// seedDirections 给五个大类钉上方向，模拟"embedding 可用时补齐种子向量"。
func seedDirections(t *testing.T, st *memnet.Store) {
	t.Helper()
	dirs := map[string]float64{
		"seed-pref":     0,
		"seed-fact":     0.5,
		"seed-how":      1.0,
		"seed-pitfall":  1.5,
		"seed-tradeoff": 2.0,
	}
	for id, a := range dirs {
		if err := st.SetNodeCentroid(id, vec(a), "test"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPrefabNetIsOnlyFiveCategories(t *testing.T) {
	st := openNet(t)

	nodes, err := st.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(nodes) != 5 {
		t.Fatalf("预制网只该有 5 个大类（第二层是派生的），实际 %d", len(nodes))
	}
	for _, n := range nodes {
		if n.Level != 1 {
			t.Fatalf("不该有第二层节点: %s L%d", n.ID, n.Level)
		}
		if n.Anchor == "" {
			t.Fatalf("枝 %s 缺种子句（空枝没有可展示的文本）", n.ID)
		}
	}

	// 再铺一次：不重复。
	if err := st.SeedNodes(time.Now()); err != nil {
		t.Fatal(err)
	}
	again, err := st.Nodes()
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 5 {
		t.Fatalf("重复铺种子不该新增枝，实际 %d", len(again))
	}

	// 新库的种子还没有向量：等上层补齐。
	pending, err := st.PendingCentroids()
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 5 {
		t.Fatalf("新库所有枝都应待补向量，实际 %d", len(pending))
	}
}

func TestAttachHomesToNearestAndCanMultiHome(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	now := time.Now()

	tune := memnet.DefaultTuning()
	tune.Center = false
	tune.LinkL1 = 0.95 // 只允许"几乎一样近"的第二枝，保证断言稳定
	st.SetTuning(tune)

	res, err := st.Attach(now, memnet.AttachInput{Content: "用户希望回复简洁", TaskID: "t1"}, vec(0.05), "test")
	if err != nil {
		t.Fatal(err)
	}
	if res.Duplicate || res.Skipped {
		t.Fatalf("首次写入应是新条目: %+v", res)
	}
	if len(res.NodeIDs) == 0 || res.NodeIDs[0] != "seed-pref" {
		t.Fatalf("主枝应是最近的大类，实际 %v", res.NodeIDs)
	}
	if len(res.NodeIDs) != 1 {
		t.Fatalf("LinkL1=0.95 时不该多挂，实际 %v", res.NodeIDs)
	}

	// 放宽到 0.4：相邻大类也够近 → 两枝都挂（这就是"网"）。
	tune.LinkL1 = 0.4
	st.SetTuning(tune)
	res2, err := st.Attach(now, memnet.AttachInput{Content: "用户希望回复简短一些", TaskID: "t2"}, vec(1.6), "test")
	if err != nil {
		t.Fatal(err)
	}
	if len(res2.NodeIDs) < 2 {
		t.Fatalf("两枝都够近时应多挂靠，实际 %v", res2.NodeIDs)
	}
}

func TestNearDuplicateOnlyCountsHits(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	now := time.Now()

	first, err := st.Attach(now, memnet.AttachInput{Content: "用户偏好：用 PowerShell 跑命令", TaskID: "t1"}, vec(1.0), "test")
	if err != nil {
		t.Fatal(err)
	}
	second, err := st.Attach(now.Add(time.Hour), memnet.AttachInput{Content: "用户偏好：用 PowerShell 跑命令", TaskID: "t2"}, vec(1.001), "test")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Duplicate || second.EntryID != first.EntryID {
		t.Fatalf("近重复应并进已有条目: %+v", second)
	}

	_, notes, total, err := st.DigNode("seed-how", now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(notes) != 1 {
		t.Fatalf("同一件事在树上只该有一条，实际 %d", total)
	}
	if notes[0].Hits != 2 {
		t.Fatalf("近重复应累加命中次数，实际 %d", notes[0].Hits)
	}
}

func TestTransientNeverEntersNet(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)

	res, err := st.Attach(time.Now(), memnet.AttachInput{
		Content:    "用户现在有点烦，这次先这样",
		Durability: "transient",
	}, vec(0), "test")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Skipped {
		t.Fatalf("一次性的情绪不该进网: %+v", res)
	}
	for _, id := range []string{"seed-pref", "seed-fact", "seed-how", "seed-pitfall", "seed-tradeoff"} {
		_, _, total, err := st.DigNode(id, time.Now(), 10)
		if err != nil {
			t.Fatal(err)
		}
		if total != 0 {
			t.Fatalf("transient 不该留在任何枝上（%s 有 %d 条）", id, total)
		}
	}
}

func TestForgettingCurve(t *testing.T) {
	st := openNet(t)
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.Local)

	// 只见过一次：30 天后掉到 1/e。
	one := st.Retention(1, "durable", now, now.Add(30*24*time.Hour))
	if math.Abs(one-math.Exp(-1)) > 1e-9 {
		t.Fatalf("S=30 天时 30 天后的留存应是 1/e，实际 %.4f", one)
	}
	if st.Retention(1, "durable", now, now.Add(90*24*time.Hour)) >= 0.15 {
		t.Fatal("90 天后单次观察应已跌破剪枝线")
	}

	// 见过三次：强度 ×4 → 同一个时间点忘得慢得多。
	if three := st.Retention(3, "durable", now, now.Add(90*24*time.Hour)); three <= 0.3 {
		t.Fatalf("命中的经验应显著更难忘，实际 %.4f", three)
	}

	// core 不衰减。
	if r := st.Retention(1, "core", now, now.Add(3650*24*time.Hour)); r != 1 {
		t.Fatalf("core 不该衰减，实际 %.4f", r)
	}
}

func TestPruneIsSoft(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.Local)

	res, err := st.Attach(base, memnet.AttachInput{Content: "旧的一次性观察"}, vec(2.0), "test")
	if err != nil {
		t.Fatal(err)
	}

	out, err := st.Prune(base.Add(120 * 24 * time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if out.Entries != 1 {
		t.Fatalf("单次观察过了两倍半衰期应被软剪枝，实际 %d", out.Entries)
	}

	// 剪掉不等于删掉：召回里看不见了。
	heads, err := st.LineHeads(nil, base.Add(121*24*time.Hour), 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 0 {
		t.Fatalf("已剪枝的条目不该出现在召回里，实际 %d 个线头", len(heads))
	}

	// 但大类仍可查（行不能删——重新遇到时要能复活）。
	if _, _, _, err := st.DigNode("seed-pitfall", base.Add(121*24*time.Hour), 10); err != nil {
		t.Fatalf("剪枝后大类仍应可查: %v", err)
	}
	if res.EntryID == "" {
		t.Fatal("条目 id 应存在")
	}
}

func TestLineHeadsClusterOnTheFly(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	tune := memnet.DefaultTuning()
	tune.Center = false
	tune.NeighborSim = 0.90 // 三条同向的互相都够近，但与第四条拉得开
	tune.NearDup = 0.99     // 别让它们被近重复合并掉
	tune.MinScore = -1      // 这条用例只验聚簇，不验过滤
	st.SetTuning(tune)
	now := time.Now()

	// 三条同向（同一件事的不同说法）+ 一条反向。
	for _, a := range []float64{1.0, 1.2, 1.4} {
		if _, err := st.Attach(now, memnet.AttachInput{Content: "seq 用进程内自增，重启就倒流", TaskID: "t"}, vec(a), "test"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.Attach(now, memnet.AttachInput{Content: "用户喜欢深色主题", TaskID: "t"}, vec(2.6), "test"); err != nil {
		t.Fatal(err)
	}

	heads, err := st.LineHeads(vec(1.0), now, 6)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) != 2 {
		t.Fatalf("应聚成 2 个线头（同向三条 + 反向一条），实际 %d", len(heads))
	}
	if heads[0].Count != 3 {
		t.Fatalf("最相关的簇应有 3 条同向经验，实际 %d", heads[0].Count)
	}
	if len(heads[0].Members) != 3 {
		t.Fatalf("线头应带回成员 id（供模型决定要不要 dig），实际 %v", heads[0].Members)
	}
	if heads[0].Text == "" {
		t.Fatal("线头要有可读的代表句")
	}
	if heads[0].Score < heads[1].Score {
		t.Fatalf("线头应按相关度排序: %+v", heads)
	}
}

func TestDigEntryPullsSiblings(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	tune := memnet.DefaultTuning()
	tune.Center = false
	tune.NeighborSim = 0.50
	st.SetTuning(tune)
	now := time.Now()

	center, err := st.Attach(now, memnet.AttachInput{Content: "PowerShell 输出被 GBK 吃掉", TaskID: "t1"}, vec(1.0), "test")
	if err != nil {
		t.Fatal(err)
	}
	// 兄弟：同大类、够近。
	if _, err := st.Attach(now, memnet.AttachInput{Content: "控制台代码页不是 UTF-8 时中文变问号", TaskID: "t1"}, vec(2.0), "test"); err != nil {
		t.Fatal(err)
	}
	// 无关：不同大类、方向也远。
	if _, err := st.Attach(now, memnet.AttachInput{Content: "用户喜欢深色主题", TaskID: "t1"}, vec(-1.0), "test"); err != nil {
		t.Fatal(err)
	}

	_, notes, total, err := st.DigEntry(center.EntryID, now, 10)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(notes) != 1 {
		t.Fatalf("只该翻出够近的兄弟，实际 %d", total)
	}
	if notes[0].Content == "" || notes[0].ID == center.EntryID {
		t.Fatalf("翻出来的应是别的条目: %+v", notes[0])
	}
}
