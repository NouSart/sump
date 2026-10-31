package server

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
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

// pptx 的 <a:br/> 是段落（<a:p>）的子元素，塞进 <a:r> 会写出需要修复的文件；
// 所以 pptxTextSpec 没有换行标签，遇到换行必须报错而不是降级写坏。
func TestReplaceInTextNodesRejectsUnsupportedBreak(t *testing.T) {
	src := `<a:p><a:r><a:t>甲</a:t></a:r></a:p>`

	_, _, err := replaceInTextNodes([]byte(src), pptxTextSpec, []textReplace{{Old: "甲", New: "一\n二"}})
	if err == nil {
		t.Fatal("pptx 的文本节点表达不了换行，应报错")
	}
	if !strings.Contains(err.Error(), "换行") {
		t.Fatalf("错误应说明换行不支持: %v", err)
	}

	// 不含换行的普通替换不受影响。
	out, counts, err := replaceInTextNodes([]byte(src), pptxTextSpec, []textReplace{{Old: "甲", New: "乙"}})
	if err != nil {
		t.Fatal(err)
	}
	if counts[0] != 1 || string(out) != `<a:p><a:r><a:t>乙</a:t></a:r></a:p>` {
		t.Fatalf("普通替换被牵连: counts=%v out=%s", counts, out)
	}
}

// pptOpReplace 在动作层就要挡住换行：这里是模型真正会打到的入口。
func TestPptOpReplaceRejectsNewline(t *testing.T) {
	doc := &pptxDoc{
		names:  []string{"ppt/slides/slide1.xml"},
		slides: map[string]string{"ppt/slides/slide1.xml": `<a:p><a:r><a:t>甲</a:t></a:r></a:p>`},
	}
	if _, err := pptOpReplace(doc, protocol.PptEditOperation{Op: "replace", Old: "甲", New: "一\n二"}); err == nil {
		t.Fatal("new 含换行时应报错")
	} else if !strings.Contains(err.Error(), "换行") {
		t.Fatalf("错误应说明换行不支持: %v", err)
	}
	if doc.slides["ppt/slides/slide1.xml"] != `<a:p><a:r><a:t>甲</a:t></a:r></a:p>` {
		t.Fatal("报错时不该改动幻灯片内容")
	}
}
