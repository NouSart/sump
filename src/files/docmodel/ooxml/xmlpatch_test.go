package ooxml

import (
	"bytes"
	"strings"
	"testing"
)

// sampleDocx 是一段"故意不好看"的正文 XML：带命名空间前缀、注释、rsid 属性、
// 属性顺序奇怪、实体引用、空元素——正是重排式修改会搞坏的那些东西。
const sampleDocx = `<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main" xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006" mc:Ignorable="w14">
  <w:body>
    <!-- 审批意见：这一行必须原样保留 -->
    <w:p w:rsidR="00A1B2C3" w:rsidRDefault="00A1B2C3">
      <w:pPr><w:pStyle w:val="Heading1"/></w:pPr>
      <w:r w:rsidRPr="00D4E5F6"><w:rPr><w:b/></w:rPr><w:t>甲方：</w:t></w:r>
      <w:r><w:t xml:space="preserve">上海某某</w:t></w:r>
      <w:r><w:t>科技有限公司</w:t></w:r>
    </w:p>
    <w:p><w:r><w:t>A &amp; B &lt;2026&gt;</w:t></w:r></w:p>
    <w:p><w:r><w:t></w:t></w:r></w:p>
  </w:body>
</w:document>`

// TestScanTilesSource 锁住核心不变量：token 区间首尾相接、完整覆盖原文。
// 这条不成立，"只替换目标字节"就无从谈起。
func TestScanTilesSource(t *testing.T) {
	data := []byte(sampleDocx)
	toks, err := Scan(data)
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	if len(toks) == 0 {
		t.Fatal("Scan 没有返回任何 token")
	}
	cur := 0
	for i, tok := range toks {
		if tok.End == tok.Start {
			continue // 自闭合元素的合成结束标签，不占字节
		}
		if tok.Start != cur {
			t.Fatalf("第 %d 个 token 起点 %d，期望 %d（原文有字节没被 token 覆盖）", i, tok.Start, cur)
		}
		cur = tok.End
	}
	if cur != len(data) {
		t.Fatalf("token 只覆盖到 %d 字节，原文 %d 字节", cur, len(data))
	}
}

// TestScanTracksNesting 校验深度与父节点：docx 的段落/run 结构就是靠它还原的。
func TestScanTracksNesting(t *testing.T) {
	toks, err := Scan([]byte(sampleDocx))
	if err != nil {
		t.Fatalf("Scan 失败: %v", err)
	}
	idx := -1
	for i, tok := range toks {
		if tok.Kind == TokStart && tok.Name == "t" {
			idx = i
			break
		}
	}
	if idx < 0 {
		t.Fatal("没找到 <w:t>")
	}
	parent := toks[idx].Parent
	if parent < 0 || toks[parent].Name != "r" {
		t.Fatalf("<w:t> 的父节点应是 w:r，实际 %+v", toks[parent])
	}
	grand := toks[parent].Parent
	if grand < 0 || toks[grand].Name != "p" {
		t.Fatalf("<w:r> 的父节点应是 w:p，实际 %+v", toks[grand])
	}
}

// TestApplyIdentity 空编辑与"改成自己"都必须逐字节不变。
func TestApplyIdentity(t *testing.T) {
	data := []byte(sampleDocx)
	out, err := Apply(data, nil)
	if err != nil {
		t.Fatalf("Apply(nil) 失败: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("空编辑改动了内容")
	}
	els, err := TextElements(data, "t")
	if err != nil {
		t.Fatalf("TextElements 失败: %v", err)
	}
	var target TextElement
	for _, el := range els {
		if el.Text == "上海某某" {
			target = el
		}
	}
	if target.Name == "" {
		t.Fatal("样例里没找到目标 run")
	}
	same := data[target.InnerStart:target.InnerEnd]
	out, err = Apply(data, []Edit{{Start: target.InnerStart, End: target.InnerEnd, Replace: same}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	if !bytes.Equal(out, data) {
		t.Fatal("替换成相同内容后字节发生了变化")
	}
}

// TestApplyOnlyTouchesTargetBytes 只改一个 <w:t> 的内容，其余字节（注释、rsid、
// 属性顺序、命名空间前缀）必须一个字节都不变。
func TestApplyOnlyTouchesTargetBytes(t *testing.T) {
	data := []byte(sampleDocx)
	els, err := TextElements(data, "t")
	if err != nil {
		t.Fatalf("TextElements 失败: %v", err)
	}
	var target TextElement
	for _, el := range els {
		if el.Text == "上海某某" {
			target = el
		}
	}
	if target.Name == "" {
		t.Fatal("样例里没找到目标 run")
	}
	out, err := Apply(data, []Edit{{Start: target.InnerStart, End: target.InnerEnd, Replace: []byte("北京某某")}})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	want := strings.Replace(sampleDocx, "上海某某", "北京某某", 1)
	if string(out) != want {
		t.Fatalf("输出与预期不符\n--- 实际 ---\n%s\n--- 预期 ---\n%s", out, want)
	}
	for _, keep := range []string{"<!-- 审批意见", `w:rsidRPr="00D4E5F6"`, `xml:space="preserve"`, `mc:Ignorable="w14"`, "<w:rPr><w:b/></w:rPr>"} {
		if !strings.Contains(string(out), keep) {
			t.Fatalf("输出丢失了原文片段：%s", keep)
		}
	}
}

// runeToByte 返回 s 中第 n 个 rune 的字节偏移（超界返回 len(s)）。
func runeToByte(s string, n int) int {
	if n <= 0 {
		return 0
	}
	i := 0
	for pos := range s {
		if i == n {
			return pos
		}
		i++
	}
	return len(s)
}

// TestApplyCrossRunSplit 覆盖 v0 的根本缺陷：Word 常把一句话拆进多个 run，
// 于是 "old 必须落在一个 <w:t> 里" 的替换匹配不到。这里按字节区间跨 run 拼出结果。
func TestApplyCrossRunSplit(t *testing.T) {
	data := []byte(sampleDocx)
	els, err := TextElements(data, "t")
	if err != nil {
		t.Fatalf("TextElements 失败: %v", err)
	}
	var first, second TextElement
	for _, el := range els {
		switch el.Text {
		case "上海某某":
			first = el
		case "科技有限公司":
			second = el
		}
	}
	if first.Name == "" || second.Name == "" {
		t.Fatal("样例里没找到跨 run 的目标")
	}
	// 目标："上海某某科技有限公司" → "上海YY有限公司"
	// ⇒ run1 改成 "上海YY"，run2 删掉开头的 "科技"。
	cut1 := runeToByte(first.Text, 2)  // "上海" 之后
	cut2 := runeToByte(second.Text, 2) // "科技" 之后
	// 注意：本样例的 run 里没有实体引用，所以解码后的偏移与原始字节偏移一致；
	// 含 &amp; 之类时要按原始片段换算（各格式层负责）。
	out, err := Apply(data, []Edit{
		{Start: first.InnerStart + cut1, End: first.InnerEnd, Replace: []byte("YY")},
		{Start: second.InnerStart, End: second.InnerStart + cut2, Replace: nil},
	})
	if err != nil {
		t.Fatalf("Apply 失败: %v", err)
	}
	got := textOf(t, out)
	want := []string{"甲方：", "上海YY", "有限公司", "A & B <2026>", ""}
	if len(got) != len(want) {
		t.Fatalf("run 数量 = %d，期望 %d：%q", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 个 run = %q，期望 %q", i, got[i], want[i])
		}
	}
	if s := strings.Join(got, ""); s != "甲方：上海YY有限公司A & B <2026>" {
		t.Fatalf("段落文本 = %q", s)
	}
}

// textOf 取出所有 <t> 的文本（测试辅助）。
func textOf(t *testing.T, data []byte) []string {
	t.Helper()
	els, err := TextElements(data, "t")
	if err != nil {
		t.Fatalf("TextElements 失败: %v", err)
	}
	out := make([]string, 0, len(els))
	for _, el := range els {
		out = append(out, el.Text)
	}
	return out
}

// TestTextElementsHandlesEmpty 空元素与自闭合元素都要算作一个 run：
// 否则"第几个 run"的编号会跟 Word 里看到的不一致。
func TestTextElementsHandlesEmpty(t *testing.T) {
	data := []byte(`<r><t>A &amp; B &lt;x&gt; &#65;&#x42;</t><t></t><t/></r>`)
	els, err := TextElements(data, "t")
	if err != nil {
		t.Fatalf("TextElements 失败: %v", err)
	}
	want := []string{"A & B <x> AB", "", ""}
	if len(els) != len(want) {
		t.Fatalf("元素数 = %d，期望 %d", len(els), len(want))
	}
	for i, el := range els {
		if el.Text != want[i] {
			t.Fatalf("第 %d 个元素文本 = %q，期望 %q", i, el.Text, want[i])
		}
		if el.InnerEnd < el.InnerStart {
			t.Fatalf("第 %d 个元素内容区间颠倒：[%d,%d)", i, el.InnerStart, el.InnerEnd)
		}
	}
	if got := string(data[els[2].Start:els[2].End]); got != "<t/>" {
		t.Fatalf("自闭合元素区间 = %q", got)
	}
}

// TestEscapeUnescapeRoundTrip 转义与还原必须严格互逆；未知实体原样保留。
func TestEscapeUnescapeRoundTrip(t *testing.T) {
	cases := []string{
		"A & B <2026>",
		`引号 "x" 与 'y'`,
		"中文 &amp; 字面",
		"<>&<>&",
		"",
	}
	for _, in := range cases {
		esc := EscapeText(in)
		if strings.ContainsAny(esc, "<>") {
			t.Fatalf("转义后仍含裸尖括号：%q", esc)
		}
		if got := UnescapeText(esc); got != in {
			t.Fatalf("往返失败：%q → %q → %q", in, esc, got)
		}
	}
	if got := UnescapeText("&#65;&#x42;&unknown;&#xZZ;"); got != "AB&unknown;&#xZZ;" {
		t.Fatalf("字符引用还原 = %q", got)
	}
}

func TestApplyRejectsBadRanges(t *testing.T) {
	data := []byte("abcdef")
	cases := []struct {
		name  string
		edits []Edit
	}{
		{"越界", []Edit{{Start: 4, End: 99, Replace: []byte("x")}}},
		{"负偏移", []Edit{{Start: -1, End: 2, Replace: []byte("x")}}},
		{"起止颠倒", []Edit{{Start: 3, End: 1, Replace: []byte("x")}}},
		{"重叠", []Edit{{Start: 0, End: 3, Replace: []byte("x")}, {Start: 2, End: 4, Replace: []byte("y")}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, err := Apply(data, c.edits); err == nil {
				t.Fatal("期望报错，实际成功")
			}
		})
	}
}
