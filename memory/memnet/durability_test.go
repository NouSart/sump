package memnet_test

// 新面：耐用度的拼写错误会被静默吞掉吗？
//
// `durability` 是**闭集**（core / durable / transient），但它是字符串从 WS 那头传进来的。
// 传 "Core" / "CORE" / "permanent" 时若被原样存下，`Stability` 只认小写的 "core"——
// 于是"不该忘的经验"会照着遗忘曲线慢慢消失，而且全程没有任何报错。
// 这正是"校验权留在服务"要管的事：宁可当场拒绝，也不要静默降级。

import (
	"testing"
	"time"

	"github.com/QAQ-awa-QAQ/sump/memory/memnet"
)

func TestDurabilityTypoIsRejected(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)

	for _, bad := range []string{"Core", "CORE", "permanent", "core "} {
		if _, err := st.Attach(time.Now(), memnet.AttachInput{Content: "某条经验", Durability: bad}, vec(1), "m"); err == nil {
			t.Fatalf("耐用度 %q 应被拒绝（拼错会静默按 durable 衰减）", bad)
		}
	}

	// 合法值与空串照常收下（空串 = 默认 durable，老调用方不用改）。
	for _, good := range []string{"", "core", "durable"} {
		if _, err := st.Attach(time.Now(), memnet.AttachInput{Content: "经验-" + good, Durability: good}, vec(1), "m"); err != nil {
			t.Fatalf("耐用度 %q 应被接受: %v", good, err)
		}
	}

	// transient 是合法值，但结果是不进网。
	res, err := st.Attach(time.Now(), memnet.AttachInput{Content: "一次性的", Durability: "transient"}, vec(1), "m")
	if err != nil || !res.Skipped {
		t.Fatalf("transient 应被接受并跳过: %+v err=%v", res, err)
	}
}

// TestCoreSurvivesWhatDurableForgets 用数据说明"静默降级"的代价：
// 同样的时间跨度，core 还完整记得，durable 已经淡到看不清。
// 这条不是为了测数学，是为了让"拼错 core 会怎样"有个具体数字。
func TestCoreSurvivesWhatDurableForgets(t *testing.T) {
	st := openNet(t)
	seedDirections(t, st)
	in := memnet.AttachInput{Content: "用户只用中文回复", Durability: "core"}
	hit := time.Now().Add(-400 * 24 * time.Hour) // 一年多以前记下的
	res, err := st.Attach(hit, in, vec(1), "m")
	if err != nil {
		t.Fatal(err)
	}
	if res.EntryID == "" {
		t.Fatal("应写进一条经验")
	}

	now := time.Now()
	const hits = 1
	if r := st.Retention(hits, "core", hit, now); r != 1 {
		t.Fatalf("core 不该衰减: %v", r)
	}
	if r := st.Retention(hits, "durable", hit, now); r > 0.01 {
		t.Fatalf("durable 过一年应该几乎忘干净（实际 %v）——这就是拼错 core 的代价", r)
	}
}
