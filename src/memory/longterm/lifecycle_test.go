package longterm

// 生命周期（遗忘曲线）与 salience/访问计数的单元测试：全是纯函数与一个小库。

import (
	"database/sql"
	"math"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestStabilityScalesWithSalienceAndAccess(t *testing.T) {
	base := Stability(Entry{Salience: 0, AccessCount: 1})
	if math.Abs(base-LifecycleBaseDays) > 1e-9 {
		t.Fatalf("新条目时间常数应为 %v，得到 %v", LifecycleBaseDays, base)
	}
	hot := Stability(Entry{Salience: 1, AccessCount: 1})
	if math.Abs(hot-LifecycleBaseDays*SalienceBoost) > 1e-6 {
		t.Fatalf("salience=1 应放大 %v 倍，得到 %v", SalienceBoost, hot)
	}
	used := Stability(Entry{Salience: 0, AccessCount: 3})
	if math.Abs(used-LifecycleBaseDays*4) > 1e-9 {
		t.Fatalf("访问 3 次应放大 4 倍，得到 %v", used)
	}
	if !math.IsInf(Stability(Entry{Priority: 1}), 1) {
		t.Fatal("核心记忆的时间常数应为 +Inf")
	}
}

func TestRetentionDecaysUnlessCore(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	old := Entry{CreatedAt: now.Add(-30 * 24 * time.Hour).Format(time.RFC3339Nano)}
	if r := Retention(old, now); math.Abs(r-math.Exp(-1)) > 1e-6 {
		t.Fatalf("30 天后保留率应约 0.3679，得到 %v", r)
	}
	fresh := Entry{LastAccessAt: now.Unix()}
	if r := Retention(fresh, now); r != 1 {
		t.Fatalf("刚访问过的条目保留率应为 1，得到 %v", r)
	}
	if r := Retention(Entry{Priority: 1, CreatedAt: old.CreatedAt}, now); r != 1 {
		t.Fatalf("核心记忆不衰减，得到 %v", r)
	}
}

func TestWeightFloorAndAccessBoost(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	ancient := Entry{CreatedAt: now.Add(-3650 * 24 * time.Hour).Format(time.RFC3339Nano)}
	w := Weight(ancient, now)
	if w < DecayFloor*0.999 || w > DecayFloor*1.001 {
		t.Fatalf("极旧条目权重应贴住下限 %v，得到 %v", DecayFloor, w)
	}
	if got := Weight(Entry{Priority: 1, CreatedAt: ancient.CreatedAt}, now); got != 1 {
		t.Fatalf("核心记忆权重应为 1，得到 %v", got)
	}
	first := Entry{CreatedAt: now.Format(time.RFC3339Nano), AccessCount: 1}
	many := Entry{CreatedAt: now.Format(time.RFC3339Nano), AccessCount: 20}
	if Weight(many, now) <= Weight(first, now) {
		t.Fatalf("访问次数应提升权重: 1 次=%v 20 次=%v", Weight(first, now), Weight(many, now))
	}
}

func TestRankLifecyclePrefersFreshOverStale(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	stale := Entry{ID: 2, Content: "杭州 天气", CreatedAt: now.Add(-200 * 24 * time.Hour).Format(time.RFC3339Nano)}
	fresh := Entry{ID: 1, Content: "杭州 天气", CreatedAt: now.Format(time.RFC3339Nano)}

	got := RankLifecycle([]Entry{stale, fresh}, "杭州", 2, 0, now)
	if len(got) != 2 || got[0].ID != fresh.ID {
		t.Fatalf("生命周期排序应把新条目排前: %+v", got)
	}
	plain := Rank([]Entry{stale, fresh}, "杭州", 2, 0)
	if len(plain) != 2 || plain[0].ID != stale.ID {
		t.Fatalf("纯相关性排序应保持 id 新近优先: %+v", plain)
	}
}

func TestRankSemanticLifecycleThresholdUsesRawCosine(t *testing.T) {
	now := time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)
	// 余弦相似度 0.9，但条目极旧：阈值只看原始余弦，不能因衰减被排除。
	e := Entry{
		ID: 1, Content: "x",
		CreatedAt: now.Add(-1000 * 24 * time.Hour).Format(time.RFC3339Nano),
		Embedding: []float32{0.9, 0.4358899},
	}
	got := RankSemanticLifecycle([]Entry{e}, []float32{1, 0}, 5, 0, 0.45, now)
	if len(got) != 1 {
		t.Fatalf("衰减不应把相关条目判成不相关: %+v", got)
	}
}

func openLongterm(t *testing.T) *Store {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "memories.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func TestAddWithSalienceMergesOnlyUpAndTouchCounts(t *testing.T) {
	st := openLongterm(t)
	id, created, err := st.AddWithSalience("身份", "住在杭州", 0, 0.3, "c1", "test")
	if err != nil || !created {
		t.Fatalf("首次写入失败: id=%d created=%v err=%v", id, created, err)
	}
	sameID, created, err := st.AddWithSalience("身份", "住在杭州", 1, 0.8, "c1", "test")
	if err != nil || created || sameID != id {
		t.Fatalf("重复内容应幂等合并: id=%d created=%v err=%v", sameID, created, err)
	}
	_, _, err = st.AddWithSalience("身份", "住在杭州", 0, 0.1, "c1", "test")
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now()
	if err := st.Touch([]int64{id, id, 0}, now); err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListActive()
	if err != nil || len(entries) != 1 {
		t.Fatalf("ListActive 不符: %+v err=%v", entries, err)
	}
	e := entries[0]
	if e.Salience != 0.8 || e.Priority != 1 {
		t.Fatalf("salience/priority 只升不降: %+v", e)
	}
	if e.AccessCount != 1 || e.LastAccessAt != now.Unix() {
		t.Fatalf("访问计数不符: %+v", e)
	}
}
