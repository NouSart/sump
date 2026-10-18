package tests

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/memory/store"
)

// TestConversationTable 验证会话表：写入即建行、列表按活跃排序、改名、回填老库。
func TestConversationTable(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// 空库：列出来是空切片，不是 nil（前端直接 map 也不会炸）。
	got, err := st.ListConversations(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Fatalf("空库应返回空切片: %+v", got)
	}

	// 落一条消息 → 会话自己就出现了（不需要谁显式建）。
	if err := st.Append("c1", "user", "你好"); err != nil {
		t.Fatal(err)
	}
	got, err = st.ListConversations(10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "c1" || got[0].Status != store.ConversationActive {
		t.Fatalf("落消息后应有会话 c1: %+v", got)
	}
	if got[0].Title != "" {
		t.Fatalf("没起名字就该是空，不造假名字: %q", got[0].Title)
	}

	// 第二个会话更活跃 → 排前面。
	if err := st.Append("c2", "user", "在吗"); err != nil {
		t.Fatal(err)
	}
	got, err = st.ListConversations(10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != "c2" {
		t.Fatalf("c2 更活跃应排前面: %+v", got)
	}

	// 入口只在空时填，不覆盖（第一次从哪进来就是哪，除非人工改库）。
	if err := st.EnsureConversation("c1", "", "qq"); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureConversation("c1", "", "shell"); err != nil {
		t.Fatal(err)
	}
	c1, err := st.ConversationByID("c1")
	if err != nil {
		t.Fatal(err)
	}
	if c1.Entry != "qq" {
		t.Fatalf("入口不该被后来者覆盖: %q", c1.Entry)
	}

	// 改名；再改成空串 = 清掉名字。
	if _, err := st.RenameConversation("c1", "第一段工作"); err != nil {
		t.Fatal(err)
	}
	if c1, err = st.ConversationByID("c1"); err != nil || c1.Title != "第一段工作" {
		t.Fatalf("改名失败: %+v err=%v", c1, err)
	}
	if _, err := st.RenameConversation("c1", ""); err != nil {
		t.Fatal(err)
	}
	if c1, err = st.ConversationByID("c1"); err != nil || c1.Title != "" {
		t.Fatalf("清名失败: %+v err=%v", c1, err)
	}

	// 不存在就报错——不悄悄造一个（调用方给错 id 是 bug，要看得见）。
	if _, err := st.RenameConversation("nope", "x"); err == nil {
		t.Fatal("给不存在的会话改名应报错")
	}
	if _, err := st.ConversationByID("nope"); err == nil {
		t.Fatal("查不存在的会话应报错")
	}

	// 归档的不在默认列表里，显式要才给。
	if err := st.Append("c3", "user", "归档我"); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE conversations SET status='archived' WHERE id='c3'`); err != nil {
		t.Fatal(err)
	}
	got, err = st.ListConversations(10, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range got {
		if c.ID == "c3" {
			t.Fatalf("归档会话不该出现在默认列表: %+v", got)
		}
	}
	all, err := st.ListConversations(10, true)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range all {
		if c.ID == "c3" {
			found = true
		}
	}
	if !found {
		t.Fatalf("显式要归档时应列出 c3: %+v", all)
	}
}

// TestConversationTitleAndLimit 验证两件容易出错的小事：
// 标题只填空不覆盖（起名是件明确的事，不能被"顺带"改掉），以及 limit 的夹紧（别让人一次拉空整库）。
func TestConversationTitleAndLimit(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.EnsureConversation("c1", "第一次起的名字", ""); err != nil {
		t.Fatal(err)
	}
	// 第二次带了个新名字：已有的名字不能被覆盖。
	if err := st.EnsureConversation("c1", "第二次起的名字", ""); err != nil {
		t.Fatal(err)
	}
	c, err := st.ConversationByID("c1")
	if err != nil {
		t.Fatal(err)
	}
	if c.Title != "第一次起的名字" {
		t.Fatalf("标题不该被后来的覆盖: %q", c.Title)
	}
	// 原来空着的，可以补上。
	if err := st.EnsureConversation("c2", "", ""); err != nil {
		t.Fatal(err)
	}
	if err := st.EnsureConversation("c2", "补上的名字", ""); err != nil {
		t.Fatal(err)
	}
	if c2, err := st.ConversationByID("c2"); err != nil || c2.Title != "补上的名字" {
		t.Fatalf("空标题应能被补上: %+v err=%v", c2, err)
	}

	// limit 夹紧：0 当默认 50，超大的当 200。造 55 个会话验证 0 → 50。
	for i := 0; i < 55; i++ {
		if err := st.EnsureConversation(fmt.Sprintf("bulk-%02d", i), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	got, err := st.ListConversations(0, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 50 {
		t.Fatalf("limit=0 应按默认 50 条，实际 %d", len(got))
	}
	// 超大的 limit 夹到上限（200），**不是**退回默认值 50——两回事，混了会悄悄少给行。
	if many, err := st.ListConversations(9999, false); err != nil || len(many) <= 50 {
		t.Fatalf("limit=9999 应夹到上限 200（当前会话数 57），不该退回默认 50：实际 %d err=%v", len(many), err)
	}
}

// TestConversationTouchThrottle 验证 updated_at 的节流：30 秒内的连续写入不重复推时间。
// 热路径每来一条消息就写一次 updated_at 纯属浪费，而且同一毫秒内的值根本区分不出来。
func TestConversationTouchThrottle(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	if err := st.Append("c1", "user", "第一句"); err != nil {
		t.Fatal(err)
	}
	first, err := st.ConversationByID("c1")
	if err != nil {
		t.Fatal(err)
	}
	// 紧接着再写两条：updated_at 不该动。
	for _, text := range []string{"第二句", "第三句"} {
		if err := st.Append("c1", "user", text); err != nil {
			t.Fatal(err)
		}
	}
	second, err := st.ConversationByID("c1")
	if err != nil {
		t.Fatal(err)
	}
	if second.UpdatedAt != first.UpdatedAt {
		t.Fatalf("30 秒内的连续写入不该推 updated_at: %q → %q", first.UpdatedAt, second.UpdatedAt)
	}
}

// TestConversationBackfill 验证老库升级：只有 messages/tasks、没有 conversations 表的老数据，
// 打开后会话列表要能照样列出来（不丢历史）。
func TestConversationBackfill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "old.db")

	// 造一个"老库"：手工建表，不建 conversations。
	old, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := old.Append("legacy-1", "user", "旧消息"); err != nil {
		t.Fatal(err)
	}
	if _, err := old.DB().Exec(`DROP TABLE conversations`); err != nil {
		t.Fatal(err)
	}
	if err := old.Close(); err != nil {
		t.Fatal(err)
	}

	// 重新打开：建表 + 回填。
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	got, err := st.ListConversations(10, false)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != "legacy-1" {
		t.Fatalf("老库的会话应被回填出来: %+v", got)
	}
	if got[0].CreatedAt == "" || got[0].UpdatedAt == "" {
		t.Fatalf("回填的时间不该是空: %+v", got[0])
	}
}
