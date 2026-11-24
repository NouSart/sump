package assemble

import "testing"

// TestVisibilityRules 覆盖 S-001 §5.2 / INV-14：默认不可见，同 work / task / thread /
// grant 四种可见路径；被过滤的记录只留 provenance。
func TestVisibilityRules(t *testing.T) {
	in := testInput()
	in.Grants = []Grant{{RefKind: "message", RefID: "h5"}}
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "本work", WorkID: "w1", Seq: 1},
		{ItemID: "h2", Role: "user", Kind: "user", Content: "别人的work", WorkID: "w2", Seq: 2},
		{ItemID: "h3", Role: "user", Kind: "user", Content: "task级", WorkID: "w2", Visibility: "task", Seq: 3},
		{ItemID: "h4", Role: "user", Kind: "user", Content: "thread级", WorkID: "w2", Visibility: "thread", Seq: 4},
		{ItemID: "h5", Role: "user", Kind: "user", Content: "被授权", WorkID: "w2", Seq: 5},
		{ItemID: "h6", Role: "user", Kind: "user", Content: "显式work", WorkID: "w2", Visibility: "work", Seq: 6},
		{ItemID: "h7", Role: "user", Kind: "user", Content: "无work", WorkID: "", Seq: 7},
	}
	out := mustAssemble(t, in)

	want := []string{"本work", "task级", "thread级", "被授权"}
	if len(out.Messages) != 1+len(want) {
		t.Fatalf("可见记录数不符，实际 %d 条消息: %+v", len(out.Messages), out.Messages)
	}
	for i, w := range want {
		if got := out.Messages[i+1].Content; got != w {
			t.Fatalf("messages[%d] = %q，want %q（顺序按 Seq）", i+1, got, w)
		}
	}
	// 不可见的三条（h2/h6/h7）不得出现，但必须留 filtered 证据。
	filtered := findProv(out, "filtered")
	if len(filtered) != 3 {
		t.Fatalf("应有 3 条 filtered provenance，实际 %+v", filtered)
	}
	gotRefs := map[string]bool{}
	for _, p := range filtered {
		if p.Index != -1 {
			t.Fatalf("filtered 记录不该进入 messages: %+v", p)
		}
		gotRefs[p.Ref] = true
	}
	for _, ref := range []string{"h2", "h6", "h7"} {
		if !gotRefs[ref] {
			t.Errorf("缺少 %s 的 filtered provenance: %+v", ref, filtered)
		}
	}
	if len(out.Warnings) != 0 {
		t.Fatalf("可见性过滤是设计内行为，不该 warning: %+v", out.Warnings)
	}
}

// TestGrantWithEmptyRefKind 兼容老数据：RefKind 为空（默认 message）也生效。
func TestGrantWithEmptyRefKind(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "被授权", WorkID: "w2", Seq: 1},
	}
	in.Grants = []Grant{{RefID: "h1"}}
	out := mustAssemble(t, in)
	if len(out.Messages) != 2 || out.Messages[1].Content != "被授权" {
		t.Fatalf("空 RefKind 的授权应视为 message 授权: %+v", out.Messages)
	}
}

// TestNonMessageGrantDoesNotLeakMessages 防止把 memory/snapshot 授权误当消息授权。
func TestNonMessageGrantDoesNotLeakMessages(t *testing.T) {
	in := testInput()
	in.History = []Record{
		{ItemID: "h1", Role: "user", Kind: "user", Content: "别人的", WorkID: "w2", Seq: 1},
	}
	in.Grants = []Grant{{RefKind: "memory", RefID: "h1"}}
	out := mustAssemble(t, in)
	if len(out.Messages) != 1 {
		t.Fatalf("memory 授权不该让消息可见: %+v", out.Messages)
	}
	if got := findProv(out, "filtered"); len(got) != 1 {
		t.Fatalf("被过滤记录应留证据: %+v", got)
	}
}
