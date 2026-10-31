package server

import (
	"strings"
	"testing"
)

// 这些用例守的是 #15 的根因：替换只能落在文本节点里，
// 一个字节都不能碰标签、属性与命名空间。

func TestReplaceInTextNodesLeavesMarkupAlone(t *testing.T) {
	src := `<r xmlns:w="ns"><w:t>abc</w:t></r>`

	// "w" 与 "t" 只出现在标签与命名空间里 → 必须报未找到。
	for _, old := range []string{"w", "t", "xmlns"} {
		if _, _, err := replaceInTextNodes([]byte(src), docxTextSpec, []textReplace{{Old: old, New: "Z"}}); err == nil {
			t.Fatalf("old=%q 只出现在标签里，不该匹配成功", old)
		}
	}

	// 正文里的字才该被替换，且标签原样保留。
	out, counts, err := replaceInTextNodes([]byte(src), docxTextSpec, []textReplace{{Old: "b", New: "Z"}})
	if err != nil {
		t.Fatal(err)
	}
	if counts[0] != 1 {
		t.Fatalf("应替换 1 处，实际 %d", counts[0])
	}
	if got := string(out); got != `<r xmlns:w="ns"><w:t>aZc</w:t></r>` {
		t.Fatalf("输出不符（标签被改动？）: %s", got)
	}
}

func TestReplaceInTextNodesExpandsNewline(t *testing.T) {
	src := `<w:r><w:t>甲</w:t></w:r>`
	out, counts, err := replaceInTextNodes([]byte(src), docxTextSpec, []textReplace{{Old: "甲", New: "一\n二"}})
	if err != nil {
		t.Fatal(err)
	}
	if counts[0] != 1 {
		t.Fatalf("应替换 1 处，实际 %d", counts[0])
	}
	got := string(out)
	want := `<w:r><w:t>一</w:t><w:br/><w:t>二</w:t></w:r>`
	if got != want {
		t.Fatalf("换行展开不符:\n got=%s\nwant=%s", got, want)
	}
}

func TestReplaceInTextNodesKeepsAttributes(t *testing.T) {
	src := `<w:r><w:t xml:space="preserve"> 甲 </w:t></w:r>`
	out, _, err := replaceInTextNodes([]byte(src), docxTextSpec, []textReplace{{Old: "甲", New: "乙\n丙"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	// 重开标签必须带上原来的 xml:space，否则首尾空格会被 Word 吃掉。
	if n := strings.Count(got, `xml:space="preserve"`); n != 2 {
		t.Fatalf("重开标签丢了属性（出现 %d 次）: %s", n, got)
	}
	if !strings.Contains(got, `<w:br/>`) {
		t.Fatalf("缺少换行标签: %s", got)
	}
}

func TestReplaceInTextNodesDoesNotDoubleEscape(t *testing.T) {
	src := `<w:t>A &amp; B</w:t>`
	out, _, err := replaceInTextNodes([]byte(src), docxTextSpec, []textReplace{{Old: "A", New: "X"}})
	if err != nil {
		t.Fatal(err)
	}
	got := string(out)
	if strings.Contains(got, "&amp;amp;") {
		t.Fatalf("实体被二次转义: %s", got)
	}
	if !strings.Contains(got, "X &amp; B") {
		t.Fatalf("结果不符: %s", got)
	}
}

func TestReplaceInTextNodesSequentialOps(t *testing.T) {
	src := `<w:t>甲乙</w:t>`
	out, counts, err := replaceInTextNodes([]byte(src), docxTextSpec, []textReplace{
		{Old: "甲", New: "丙"},
		{Old: "乙", New: "丁"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if counts[0] != 1 || counts[1] != 1 {
		t.Fatalf("各操作计数不符: %v", counts)
	}
	if got := string(out); got != `<w:t>丙丁</w:t>` {
		t.Fatalf("结果不符: %s", got)
	}
}
