package memnet_test

// 近重复只加命中，**不改向量**。
//
// 曾经是覆盖：`reinforce` 把旧条目的 embedding 换成这次观测的向量。问题在于近重复阈值很松
// （0.62）——两条"看着像"的观察就能悄悄改写旧条目的几何，而它的 content 还是原来那句。
// 内容与向量从此对不上，召回会越飘越远。这条用例钉住"内容不变，向量也不变"。

import (
	"testing"
	"time"

	"github.com/NouSart/sump/memory/memnet"
)

func TestReinforceKeepsVector(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	// 关掉中心化：二维假向量减均值后角度会乱跑，断言就没法直白地算了。
	tune := memnet.DefaultTuning()
	tune.Center = false
	st.SetTuning(tune)

	original := vec(0.3)
	res, err := st.Attach(time.Now(), memnet.AttachInput{Content: "同一个坑"}, original, "m")
	if err != nil {
		t.Fatal(err)
	}

	// 同一件事的另一种说法：转了 0.7 弧度（余弦约 0.76，超过 0.62 → 算同一件事）。
	again := vec(1.0)
	dup, err := st.Attach(time.Now(), memnet.AttachInput{Content: "同一个坑"}, again, "m")
	if err != nil {
		t.Fatal(err)
	}
	if !dup.Duplicate || dup.EntryID != res.EntryID {
		t.Fatalf("这句话应被认成同一件事: first=%+v dup=%+v", res, dup)
	}

	notes, _, _, err := st.DigEntry(res.EntryID, time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if notes.Content != "同一个坑" {
		t.Fatalf("条目应还在: %+v", notes)
	}
	if notes.Hits != 2 {
		t.Fatalf("重复遇到应加命中，实际 %d", notes.Hits)
	}

	// 关键断言：向量没被换成"另一种说法"。用 again 当查询时，分数应仍是原向量与它的余弦
	//（≈0.7648），而不是 1（那样说明向量被覆盖了）。
	heads, err := st.LineHeads(again, time.Now(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(heads) == 0 {
		t.Fatal("应能给出一条线头")
	}
	if got := heads[0].Score; got < 0.70 || got > 0.82 {
		t.Fatalf("条目向量被改写了：分数 %.4f（应≈0.7648；≈1 表示被覆盖）", got)
	}
}
