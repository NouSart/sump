package tests

import (
	"path/filepath"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/memory/store"
)

// TestStoreAppendRecent 验证落库、幂等与按会话查询。
func TestStoreAppendRecent(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, c := range []string{"a", "b", "c", "d"} {
		if err := st.Append("c1", "user", c); err != nil {
			t.Fatal(err)
		}
	}
	// 幂等：与上一条完全相同的消息不重复入库。
	if err := st.Append("c1", "user", "d"); err != nil {
		t.Fatal(err)
	}
	if err := st.Append("c2", "user", "x"); err != nil {
		t.Fatal(err)
	}

	got, err := st.Recent("c1", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Content != "b" || got[1].Content != "c" || got[2].Content != "d" {
		t.Fatalf("Recent(c1,3) 不符: %+v", got)
	}

	all, err := st.Recent("c1", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].Content != "a" {
		t.Fatalf("Recent(c1,10) 不符: %+v", all)
	}

	other, err := st.Recent("c2", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(other) != 1 || other[0].Content != "x" {
		t.Fatalf("Recent(c2,10) 不符: %+v", other)
	}
}

// TestStoreHistory 验证 history：按时间正序返回、带落库时间、limit 生效、不跨会话。
func TestStoreHistory(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, c := range []string{"一", "二", "三"} {
		if err := st.Append("h1", "user", c); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.Append("h2", "user", "别的会话"); err != nil {
		t.Fatal(err)
	}

	got, err := st.History("h1", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Content != "二" || got[1].Content != "三" {
		t.Fatalf("History(h1,2) 不符: %+v", got)
	}
	for _, m := range got {
		if m.Role != "user" || m.At == "" {
			t.Fatalf("History 每条都该带 role 与落库时间: %+v", m)
		}
	}

	all, err := st.History("h1", 0) // 0 → 默认 50
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Content != "一" || all[2].Content != "三" {
		t.Fatalf("History(h1,0) 不符: %+v", all)
	}
}
