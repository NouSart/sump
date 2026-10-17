package tests

import (
	"database/sql"
	"path/filepath"
	"strings"
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

	got, err := st.Recent("c1", 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Content != "b" || got[1].Content != "c" || got[2].Content != "d" {
		t.Fatalf("Recent(c1,3) 不符: %+v", got)
	}

	all, err := st.Recent("c1", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 || all[0].Content != "a" {
		t.Fatalf("Recent(c1,10) 不符: %+v", all)
	}

	other, err := st.Recent("c2", 10, "")
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

	got, err := st.History("h1", 2, 0, "")
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

	all, err := st.History("h1", 0, 0, "") // 0 → 默认 100
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 || all[0].Content != "一" || all[2].Content != "三" {
		t.Fatalf("History(h1,0) 不符: %+v", all)
	}
}

// TestStoreRecords LLM 轨迹记录：kind/meta/seq 原样落库，按 seq 排序，思考与工具不进 Recent。
func TestStoreRecords(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Record("c", "user", "跑个命令", "", "", 1, ""); err != nil {
		t.Fatal(err)
	}
	// 并发写乱序到达：seq 决定先后，落库顺序不影响结果
	if err := st.Record("c", "assistant", "Get-Date", "tool_call", `{"tool":"shell__run","command":"Get-Date"}`, 3, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Record("c", "assistant", "想一下", "reasoning", "", 2, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Record("c", "tool", "2026-10-17", "tool_result", `{"tool":"shell__run"}`, 4, ""); err != nil {
		t.Fatal(err)
	}

	hist, err := st.History("c", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	order := make([]string, 0, len(hist))
	for _, m := range hist {
		order = append(order, m.Kind)
	}
	want := []string{"user", "reasoning", "tool_call", "tool_result"}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Fatalf("history 顺序不对: %v", order)
	}
	if hist[2].Meta == "" || hist[2].Seq != 3 {
		t.Fatalf("meta/seq 没落库: %+v", hist[2])
	}

	// 增量：只要 seq > 2 的
	tail, err := st.History("c", 0, 2, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(tail) != 2 || tail[0].Kind != "tool_call" || tail[1].Kind != "tool_result" {
		t.Fatalf("after_seq 增量不对: %+v", tail)
	}

	// 进 LLM 上下文的只有对话消息
	recent, err := st.Recent("c", 10, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(recent) != 1 || recent[0].Content != "跑个命令" {
		t.Fatalf("Recent 应只含 user/assistant: %+v", recent)
	}
}

// TestStoreNoDedupeForEvents 事件类记录不去重：连跑两条一模一样的命令是两件事。
func TestStoreNoDedupeForEvents(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for i := 0; i < 2; i++ {
		if err := st.Record("c", "assistant", "Get-Date", "tool_call", "", int64(i+1), ""); err != nil {
			t.Fatal(err)
		}
	}
	hist, err := st.History("c", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 {
		t.Fatalf("两条相同命令都该落库，实际 %d 条", len(hist))
	}
	// 对话消息仍去重
	_ = st.Append("c", "user", "你好")
	_ = st.Append("c", "user", "你好")
	hist, _ = st.History("c", 0, 0, "")
	if len(hist) != 3 {
		t.Fatalf("重复的用户消息该被压掉，实际 %d 条", len(hist))
	}
}

// TestStoreMigrateOldSchema 老库（没有 kind/meta/seq 列）能平滑升级，老数据按 role 推断。
func TestStoreMigrateOldSchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE messages (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		conversation_id TEXT NOT NULL, role TEXT NOT NULL, content TEXT NOT NULL, created_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("INSERT INTO messages(conversation_id, role, content, created_at) VALUES('c','user','老消息','2026-01-01T00:00:00Z')"); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	st, err := store.Open(path)
	if err != nil {
		t.Fatalf("老库升级失败: %v", err)
	}
	defer st.Close()
	hist, err := st.History("c", 0, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 1 || hist[0].Content != "老消息" || hist[0].Kind != "user" {
		t.Fatalf("老数据读出来不对: %+v", hist)
	}
}