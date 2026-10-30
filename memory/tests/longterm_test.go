package tests

// 长期记忆 v0 单元测试：条目写入 / 幂等 / 软删 / 旧表迁移；
// bigram 检索（1~2 字中文词必须命中——FTS5 trigram 会打空）/ 排序 / 预算 / 核心注入。

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/store"
)

// openLT 打开一套临时库（会话库 + 共享句柄的长期库）。
func openLT(t *testing.T) *longterm.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	lt, err := longterm.Open(st.DB())
	if err != nil {
		t.Fatal(err)
	}
	return lt
}

// TestLongtermAddForget 验证写入、幂等（同 kind+content 跳过）、软删。
func TestLongtermAddForget(t *testing.T) {
	lt := openLT(t)

	id, created, err := lt.Add("偏好", "主人喜欢喝美式咖啡，不加糖", 0, "", "test")
	if err != nil || id <= 0 || !created {
		t.Fatalf("首次写入不符: id=%d created=%v err=%v", id, created, err)
	}
	// 幂等：同 kind+content 已有 active 条目 → 跳过（即使 priority 不同）。
	id2, created2, err := lt.Add("偏好", "主人喜欢喝美式咖啡，不加糖", 5, "", "test")
	if err != nil || created2 || id2 != id {
		t.Fatalf("幂等跳过不符: id=%d created=%v err=%v", id2, created2, err)
	}

	entries, err := lt.ListActive()
	if err != nil || len(entries) != 1 || entries[0].Content != "主人喜欢喝美式咖啡，不加糖" {
		t.Fatalf("ListActive 不符: %+v err=%v", entries, err)
	}

	ok, err := lt.Forget(id)
	if err != nil || !ok {
		t.Fatalf("软删失败: ok=%v err=%v", ok, err)
	}
	entries, _ = lt.ListActive()
	if len(entries) != 0 {
		t.Fatalf("软删后不应还有 active 条目: %+v", entries)
	}
	if ok, _ := lt.Forget(id); ok {
		t.Fatal("重复软删应返回 false")
	}
	// 软删后可重新写入（旧行保留为 deleted）。
	id3, created3, err := lt.Add("偏好", "主人喜欢喝美式咖啡，不加糖", 0, "", "test")
	if err != nil || !created3 || id3 == id {
		t.Fatalf("软删后重写不符: id=%d created=%v err=%v", id3, created3, err)
	}
}

// TestLongtermMigration 旧占位表：空表自动重建；有数据则拒绝自动迁移。
func TestLongtermMigration(t *testing.T) {
	const oldShape = `CREATE TABLE memories (id INTEGER PRIMARY KEY AUTOINCREMENT, conversation_id TEXT, kind TEXT, content TEXT, created_at TEXT)`

	// 空旧表 → 自动重建。
	st, err := store.Open(filepath.Join(t.TempDir(), "a.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.DB().Exec(oldShape); err != nil {
		t.Fatal(err)
	}
	lt, err := longterm.Open(st.DB())
	if err != nil {
		t.Fatalf("空旧表应自动重建: %v", err)
	}
	if _, _, err := lt.Add("知识", "重建后可写", 0, "", "test"); err != nil {
		t.Fatalf("重建后写入失败: %v", err)
	}

	// 有数据的旧表 → 报错（人工处理）。
	st2, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	if _, err := st2.DB().Exec(oldShape); err != nil {
		t.Fatal(err)
	}
	if _, err := st2.DB().Exec(`INSERT INTO memories(conversation_id, kind, content, created_at) VALUES('c','k','v','t')`); err != nil {
		t.Fatal(err)
	}
	if _, err := longterm.Open(st2.DB()); err == nil {
		t.Fatal("有数据的旧占位表应拒绝自动迁移")
	}
}

// TestTerms 验证查询词提取：汉字 bigram / 孤立单字 / ASCII 整词 / 去重。
func TestTerms(t *testing.T) {
	got := longterm.Terms("我明天想喝杯咖啡")
	want := []string{"我明", "明天", "天想", "想喝", "喝杯", "杯咖", "咖啡"}
	if len(got) != len(want) {
		t.Fatalf("Terms 数量不符: %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Terms[%d] = %q, want %q（%v）", i, got[i], want[i], got)
		}
	}

	got = longterm.Terms("QQ 的 token 放哪了")
	has := func(terms []string, t string) bool {
		for _, v := range terms {
			if v == t {
				return true
			}
		}
		return false
	}
	if !has(got, "qq") || !has(got, "token") {
		t.Fatalf("ASCII 词提取不符: %v", got)
	}

	if got := longterm.Terms("猫"); len(got) != 1 || got[0] != "猫" {
		t.Fatalf("孤立单字提取不符: %v", got)
	}
}

// TestRank 验证 bigram 召回：2 字中文词命中、多命中优先、预算裁剪。
func TestRank(t *testing.T) {
	entries := []longterm.Entry{
		{ID: 1, Kind: "偏好", Content: "主人喜欢喝美式咖啡，不加糖"},
		{ID: 2, Kind: "事件", Content: "家里猫叫团子，怕打雷"},
		{ID: 3, Kind: "知识", Content: "每周三晚上要开例会"},
	}

	// 2 字词（FTS5 trigram 打空的情形）——bigram 必须命中。
	if got := longterm.Rank(entries, "咖啡", 5, 800); len(got) != 1 || got[0].ID != 1 {
		t.Fatalf("2 字词召回不符: %+v", got)
	}
	if got := longterm.Rank(entries, "团子", 5, 800); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("2 字词召回不符: %+v", got)
	}
	if got := longterm.Rank(entries, "zzz", 5, 800); len(got) != 0 {
		t.Fatalf("无关查询不应命中: %+v", got)
	}

	// 多命中优先。
	duo := []longterm.Entry{
		{ID: 11, Kind: "k", Content: "咖啡"},
		{ID: 12, Kind: "k", Content: "美式咖啡好喝"},
	}
	got := longterm.Rank(duo, "美式咖啡", 5, 800)
	if len(got) != 2 || got[0].ID != 12 {
		t.Fatalf("多命中应排前: %+v", got)
	}

	// 条数上限与字符预算（超长条目跳过）。
	long := strings.Repeat("咖啡", 200) // 400 字
	budget := []longterm.Entry{
		{ID: 21, Kind: "k", Content: long},
		{ID: 22, Kind: "k", Content: "咖啡好喝"},
	}
	got = longterm.Rank(budget, "咖啡", 5, 100)
	if len(got) != 1 || got[0].ID != 22 {
		t.Fatalf("预算裁剪不符: %+v", got)
	}
	if got = longterm.Rank(entries, "咖啡", 0, 800); len(got) != 1 {
		t.Fatalf("limit=0（不限）不符: %+v", got)
	}
}

// TestCore 验证核心条目（priority>0）：按优先级排序、受条数与预算约束。
func TestCore(t *testing.T) {
	entries := []longterm.Entry{
		{ID: 1, Kind: "身份", Content: "猫叫团子", Priority: 1},
		{ID: 2, Kind: "身份", Content: "主人叫某某", Priority: 2},
		{ID: 3, Kind: "偏好", Content: "喜欢咖啡", Priority: 0},
	}
	got := longterm.Core(entries, 3, 400)
	if len(got) != 2 || got[0].ID != 2 || got[1].ID != 1 {
		t.Fatalf("核心排序不符: %+v", got)
	}
	if got = longterm.Core(entries, 1, 400); len(got) != 1 || got[0].ID != 2 {
		t.Fatalf("核心条数上限不符: %+v", got)
	}
	if got = longterm.Core(entries, 3, 8); len(got) != 1 {
		t.Fatalf("核心字符预算不符: %+v", got)
	}
}
