// 归档口径的包内测试。
//
// 这条契约原先只在 src/memory/tests 的服务级测试里钉过，store 包自己一条都没有。
// 异变测试抓出了后果：把 `if !includeArchived` 改成永不成立，被改的包是 store，
// 工具跑的却是 `go test ./store`——全绿。"归档的会话冒回默认列表"这种回归，
// 于是只有把整条服务链拉起来才会响。这里把铃铛挪到离改动最近的那一层。
package store

import (
	"testing"

	"github.com/NouSart/sump/protocol"
)

func listIDs(cs []protocol.Conversation) []string {
	out := make([]string, 0, len(cs))
	for _, c := range cs {
		out = append(out, c.ID)
	}
	return out
}

func hasID(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestArchivedOnlyLeavesTheDefaultList(t *testing.T) {
	s := openTemp(t)
	for _, id := range []string{"c1", "c2"} {
		if err := s.EnsureConversation(id, "", ""); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SetConversationStatus("c2", true); err != nil {
		t.Fatal(err)
	}

	got, err := s.ListConversations(10, false)
	if err != nil {
		t.Fatal(err)
	}
	if ids := listIDs(got); hasID(ids, "c2") || !hasID(ids, "c1") {
		t.Errorf("默认列表 = %v，想要只含 c1（归档的 c2 不许冒回来）", ids)
	}

	all, err := s.ListConversations(10, true)
	if err != nil {
		t.Fatal(err)
	}
	if ids := listIDs(all); !hasID(ids, "c2") || !hasID(ids, "c1") {
		t.Errorf("含归档列表 = %v，想要 c1 与 c2 都在（归档不是删除）", ids)
	}

	// 归档不拦写：写进去的照旧落库，状态也不会被"顺带"洗回 active。
	if err := s.Record("c2", "user", "归档之后又说一句", "user", "", 1, ""); err != nil {
		t.Fatalf("归档会话应可继续写入: %v", err)
	}
	c, err := s.ConversationByID("c2")
	if err != nil {
		t.Fatal(err)
	}
	if c.Status != ConversationArchived {
		t.Errorf("c2 状态 = %q，写入后仍是 %q 才对", c.Status, ConversationArchived)
	}
	if got, err = s.ListConversations(10, false); err != nil {
		t.Fatal(err)
	}
	if hasID(listIDs(got), "c2") {
		t.Errorf("默认列表 = %v，写入不该把归档会话拉回来", listIDs(got))
	}

	// 取消归档放回默认列表——两半都要钉住，否则只验了"进"没验"出"。
	if _, err := s.SetConversationStatus("c2", false); err != nil {
		t.Fatal(err)
	}
	if got, err = s.ListConversations(10, false); err != nil {
		t.Fatal(err)
	}
	if !hasID(listIDs(got), "c2") {
		t.Errorf("取消归档后默认列表 = %v，c2 应回来", listIDs(got))
	}
}
