package server

// 整理决策解析与提示词边界：全是纯函数，不碰网络与数据库。

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/memory/longterm"
	"github.com/NouSart/sump/memory/store"
)

func TestParseConsolidationDecisionsAcceptsFences(t *testing.T) {
	text := "结果如下：\n```json\n" + `[
		{"id":1,"keep":true,"kind":" 身份 ","content":"住在杭州","salience":0.7,"priority":1,"durability":"core","reason":"稳定事实",
		 "conflicts":[{"id":7,"resolution":"keep_new","reason":"旧说法已过时"}]},
		{"id":2,"keep":false}
	]` + "\n```"
	decisions, err := parseConsolidationDecisions(text, map[int64]bool{1: true, 2: true}, map[int64]bool{7: true})
	if err != nil {
		t.Fatalf("合法输出不应报错: %v", err)
	}
	if len(decisions) != 2 || decisions[0].Kind != "身份" || decisions[0].Durability != "core" {
		t.Fatalf("解析结果不符: %+v", decisions)
	}
	if len(decisions[0].Conflicts) != 1 || decisions[0].Conflicts[0].ID != 7 {
		t.Fatalf("冲突解析不符: %+v", decisions[0].Conflicts)
	}
}

func TestParseConsolidationDecisionsRejectsBadOutput(t *testing.T) {
	valid := map[int64]bool{1: true, 2: true}
	existing := map[int64]bool{7: true}
	cases := []struct {
		name string
		text string
	}{
		{"没有数组", "模型坏了"},
		{"未知候选", `[{"id":9,"keep":false}]`},
		{"重复候选", `[{"id":1,"keep":false},{"id":1,"keep":false}]`},
		{"空决定", `[]`},
		{"keep 但 kind 为空", `[{"id":1,"keep":true,"content":"x","salience":0.5}]`},
		{"keep 但 content 为空", `[{"id":1,"keep":true,"kind":"身份","content":"  ","salience":0.5}]`},
		{"salience 越界", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":1.5}]`},
		{"priority 越界", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":0.5,"priority":4}]`},
		{"durability 非法", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":0.5,"durability":"forever"}]`},
		{"冲突未知", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":0.5,"conflicts":[{"id":9,"resolution":"keep_new"}]}]`},
		{"冲突重复", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":0.5,"conflicts":[{"id":7,"resolution":"keep_new"},{"id":7,"resolution":"keep_both"}]}]`},
		{"keep_old 与 keep_new 并存", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":0.5,"conflicts":[{"id":7,"resolution":"keep_old"},{"id":7,"resolution":"keep_new"}]}]`},
		{"keep_new 与 keep_both 并存", `[{"id":1,"keep":true,"kind":"身份","content":"x","salience":0.5,"conflicts":[{"id":7,"resolution":"keep_new"},{"id":7,"resolution":"keep_both"}]}]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := parseConsolidationDecisions(tc.text, valid, existing); err == nil {
				t.Fatalf("应拒绝：%s", tc.text)
			}
		})
	}
}

func TestConsolidationPromptIsBounded(t *testing.T) {
	items := make([]store.StagedItem, 0, 100)
	for i := 1; i <= 100; i++ {
		items = append(items, store.StagedItem{
			ID: int64(i), Role: "user",
			Content: strings.Repeat("啊", 2000),
		})
	}
	existing := make([]longterm.Entry, 0, 100)
	for i := 1; i <= 100; i++ {
		existing = append(existing, longterm.Entry{ID: int64(i), Kind: "身份", Content: "旧记忆"})
	}
	prompt := consolidationPrompt(items, existing)
	if len(prompt) > consolidateMaxPrompt+2000 {
		t.Fatalf("提示词应有界，实际 %d 字符", len(prompt))
	}
	if !strings.Contains(prompt, "已有长期记忆") {
		t.Fatal("提示词应包含已有长期记忆小节")
	}
}
