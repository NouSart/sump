package docx

// docx 层的核心验收：结构地址稳定、文本投影正确、跨 run 替换只动目标字节。
// 夹具用真实 Word 会写出的形状（多 run 拆句、<w:br/>、表格、<w:p/> 空段）。

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/QAQ-awa-QAQ/sump/files/docmodel"
	"github.com/QAQ-awa-QAQ/sump/files/docmodel/ooxml"
)

const testNS = `xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"`

// docxBody 用给定 body 片段拼出最小 .docx（多带两个非目标 part，用来验字节保真）。
func docxBody(t *testing.T, body string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	write := func(name, data string) {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatalf("创建 zip 条目 %s 失败: %v", name, err)
		}
		if _, err := w.Write([]byte(data)); err != nil {
			t.Fatalf("写入 zip 条目 %s 失败: %v", name, err)
		}
	}
	write("[Content_Types].xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types"><Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/><Default Extension="xml" ContentType="application/xml"/><Override PartName="/word/document.xml" ContentType="application/vnd.openxmlformats-officedocument.wordprocessingml.document.main+xml"/></Types>`)
	write("_rels/.rels", `<?xml version="1.0" encoding="UTF-8"?><Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships"><Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="word/document.xml"/></Relationships>`)
	write("word/document.xml", `<?xml version="1.0" encoding="UTF-8" standalone="yes"?><w:document `+testNS+`><w:body>`+body+`<w:sectPr/></w:body></w:document>`)
	write("word/styles.xml", `<?xml version="1.0" encoding="UTF-8"?><w:styles `+testNS+`><w:style w:type="paragraph" w:styleId="Heading1"/></w:styles>`)
	write("docProps/core.xml", `<?xml version="1.0" encoding="UTF-8"?><cp:coreProperties xmlns:cp="http://schemas.openxmlformats.org/package/2006/metadata/core-properties"><cp:lastModifiedBy>测试</cp:lastModifiedBy></cp:coreProperties>`)
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 zip 失败: %v", err)
	}
	return buf.Bytes()
}

// partOf 从一份 docx 字节里取出某个 part（用于断言改后的 XML 形状）。
func partOf(t *testing.T, data []byte, name string) string {
	t.Helper()
	pkg, err := ooxml.Open(data, ooxml.DefaultBudget)
	if err != nil {
		t.Fatalf("打开容器失败: %v", err)
	}
	b, err := pkg.Part(name)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", name, err)
	}
	return string(b)
}

func mustOpen(t *testing.T, data []byte) *Document {
	t.Helper()
	d, err := Open(data)
	if err != nil {
		t.Fatalf("打开 docx 失败: %v", err)
	}
	return d
}

// TestOutlineAndProjection 段落 ID 按结构顺序、标题层级来自 pStyle。
func TestOutlineAndProjection(t *testing.T) {
	src := docxBody(t, `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>第一章 总则</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>正文第一段</w:t></w:r></w:p>`)
	d := mustOpen(t, src)

	if got, want := d.Text(), "第一章 总则\n正文第一段"; got != want {
		t.Fatalf("整篇文本 = %q，期望 %q", got, want)
	}
	if got := d.paras["body/p1"].text; got != "第一章 总则" {
		t.Fatalf("body/p1 = %q", got)
	}
	outline, err := d.Outline(context.Background(), 1)
	if err != nil {
		t.Fatalf("outline 失败: %v", err)
	}
	if len(outline.Nodes) != 2 {
		t.Fatalf("outline 节点数 = %d，期望 2", len(outline.Nodes))
	}
	if outline.Nodes[0].ID != "body/p1" || outline.Nodes[0].Level != 1 {
		t.Fatalf("首节点 = %+v，期望 body/p1 + level 1", outline.Nodes[0])
	}
	if outline.Nodes[1].ID != "body/p2" || outline.Nodes[1].Level != 0 {
		t.Fatalf("次节点 = %+v", outline.Nodes[1])
	}
	if want := len([]rune("第一章 总则")) + len([]rune("正文第一段")); outline.Stats.Chars != want {
		t.Fatalf("chars = %d，期望 %d", outline.Stats.Chars, want)
	}
	if outline.Stats.Parts < 3 {
		t.Fatalf("parts = %d，夹具应有 ≥3 个 part", outline.Stats.Parts)
	}
}

// TestFindAndReplaceAcrossRuns 是 P0 的招牌场景：一句话被 Word 拆成两个 run，
// 按字符区间替换必须能跨 run 改，且不新造 run。
func TestFindAndReplaceAcrossRuns(t *testing.T) {
	src := docxBody(t, `<w:p><w:r><w:t>上海某某</w:t></w:r><w:r><w:t>科技有限公司</w:t></w:r></w:p>`)
	d := mustOpen(t, src)
	if got := d.paras["body/p1"].text; got != "上海某某科技有限公司" {
		t.Fatalf("投影文本 = %q", got)
	}

	matches, err := d.Find(context.Background(), docmodel.Query{Text: "科技有限公司"})
	if err != nil {
		t.Fatalf("find 失败: %v", err)
	}
	if len(matches) != 1 {
		t.Fatalf("命中数 = %d，期望 1", len(matches))
	}
	m := matches[0]
	if m.Node != "body/p1" || m.Run != "body/p1/r2" || m.Start != 4 || m.End != 10 {
		t.Fatalf("命中 = %+v，期望 body/p1 / body/p1/r2 / [4,10)", m)
	}

	res, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op:    docmodel.OpReplaceText,
		Node:  "body/p1",
		Range: &docmodel.TextRange{Start: 2, End: 8}, // 某某科技有限（跨两个 run）
		Text:  "YY",
	}})
	if err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	if res.Applied != 1 || len(res.Diff) != 1 {
		t.Fatalf("patch 结果 = %+v", res)
	}
	if got := d.paras["body/p1"].text; got != "上海YY公司" {
		t.Fatalf("改后文本 = %q，期望 上海YY公司", got)
	}
	xml := partOf(t, d.Bytes(), "word/document.xml")
	for _, want := range []string{"<w:t>上海YY</w:t>", "<w:t>公司</w:t>"} {
		if !strings.Contains(xml, want) {
			t.Fatalf("改后的 document.xml 缺少 %s：\n%s", want, xml)
		}
	}
	if n := strings.Count(xml, "<w:r>"); n != 2 {
		t.Fatalf("run 数 = %d，期望仍是 2（不应新造 run）", n)
	}
}

// TestBreakAndTabProjection <w:br/> → "\n"、<w:tab/> → "\t"，替换时整元素删除/保留。
func TestBreakAndTabProjection(t *testing.T) {
	src := docxBody(t, `<w:p><w:r><w:t>甲</w:t><w:br/><w:t>乙</w:t><w:tab/><w:t>丙</w:t></w:r></w:p>`)
	d := mustOpen(t, src)
	if got := d.paras["body/p1"].text; got != "甲\n乙\t丙" {
		t.Fatalf("投影文本 = %q，期望 %q", got, "甲\n乙\t丙")
	}

	// 用 "X" 吃掉那个换行：原 <w:br/> 必须消失，新文本落到前一个 <w:t> 里。
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op:    docmodel.OpReplaceText,
		Node:  "body/p1",
		Range: &docmodel.TextRange{Start: 1, End: 2},
		Text:  "X",
	}}); err != nil {
		t.Fatalf("替换换行失败: %v", err)
	}
	if got := d.paras["body/p1"].text; got != "甲X乙\t丙" {
		t.Fatalf("改后文本 = %q，期望 %q", got, "甲X乙\t丙")
	}
	xml := partOf(t, d.Bytes(), "word/document.xml")
	if strings.Contains(xml, "<w:br/>") {
		t.Fatalf("被替换掉的 <w:br/> 还在：%s", xml)
	}
	if !strings.Contains(xml, "<w:tab/>") {
		t.Fatalf("<w:tab/> 不该被删：%s", xml)
	}
}

// TestIdentityPatchKeepsBytes 内容没变时必须零字节差异（不重新打包）。
func TestIdentityPatchKeepsBytes(t *testing.T) {
	src := docxBody(t, `<w:p><w:r><w:t>原文</w:t></w:r></w:p>`)
	d := mustOpen(t, src)
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op:   docmodel.OpReplaceText,
		Node: "body/p1",
		Text: "原文",
	}}); err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	if !bytes.Equal(d.Bytes(), src) {
		t.Fatalf("内容未变却重写了文件（%d → %d 字节）", len(src), len(d.Bytes()))
	}
}

// TestNonTargetPartsByteIdentical 改正文时，别的 part 必须逐字节不变。
func TestNonTargetPartsByteIdentical(t *testing.T) {
	src := docxBody(t, `<w:p><w:r><w:t>旧标题</w:t></w:r></w:p>`)
	d := mustOpen(t, src)
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op:   docmodel.OpReplaceText,
		Node: "body/p1",
		Text: "新标题",
	}}); err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	out := d.Bytes()
	for _, name := range []string{"word/styles.xml", "docProps/core.xml", "[Content_Types].xml", "_rels/.rels"} {
		if got, want := partOf(t, out, name), partOf(t, src, name); got != want {
			t.Fatalf("非目标 part %s 被改动了:\n改后=%q\n原来=%q", name, got, want)
		}
	}
}

// TestTableCellAddressing 表格单元格里的段落有独立地址，能按地址改。
func TestTableCellAddressing(t *testing.T) {
	src := docxBody(t, `<w:tbl><w:tr><w:tc><w:p><w:r><w:t>单价</w:t></w:r></w:p></w:tc>`+
		`<w:tc><w:p><w:r><w:t>100 元</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`)
	d := mustOpen(t, src)
	cell := docmodel.NodeID("body/tbl1/r1/c2/p1")
	if got := d.paras[cell].text; got != "100 元" {
		t.Fatalf("%s = %q", cell, got)
	}
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op:   docmodel.OpReplaceText,
		Node: cell,
		Text: "120 元",
	}}); err != nil {
		t.Fatalf("改单元格失败: %v", err)
	}
	if got := d.paras[cell].text; got != "120 元" {
		t.Fatalf("改后 %s = %q", cell, got)
	}
	// 另一格不能被带着改。
	if got := d.paras["body/tbl1/r1/c1/p1"].text; got != "单价" {
		t.Fatalf("同行的另一格被改了: %q", got)
	}
	outline, err := d.Outline(context.Background(), 2)
	if err != nil {
		t.Fatalf("outline 失败: %v", err)
	}
	if len(outline.Nodes) != 1 || len(outline.Nodes[0].Children) != 1 {
		t.Fatalf("depth=2 的表格应展开成 1 行：%+v", outline.Nodes)
	}
}

// TestPatchRejectsStaleAnchorAndBadNode anchor 不匹配 / 节点不存在 / 不支持的 op
// 都必须整批拒绝，且文档字节不变。
func TestPatchRejectsStaleAnchorAndBadNode(t *testing.T) {
	src := docxBody(t, `<w:p><w:r><w:t>合同金额 100 元</w:t></w:r></w:p>`)
	d := mustOpen(t, src)
	good := docmodel.Fingerprint("合同金额 100 元")

	cases := []struct {
		name string
		op   docmodel.PatchOp
		want error
	}{
		{"anchor 过期", docmodel.PatchOp{Op: docmodel.OpReplaceText, Node: "body/p1", Anchor: "deadbeefdeadbeef", Text: "x"}, docmodel.ErrStale},
		{"节点不存在", docmodel.PatchOp{Op: docmodel.OpReplaceText, Node: "body/p99", Text: "x"}, docmodel.ErrNodeNotFound},
		{"区间越界", docmodel.PatchOp{Op: docmodel.OpReplaceText, Node: "body/p1", Range: &docmodel.TextRange{Start: 0, End: 999}, Text: "x"}, docmodel.ErrBadOp},
		{"op 不支持", docmodel.PatchOp{Op: docmodel.OpInsertParagraph, Node: "body/p1", Text: "x"}, docmodel.ErrBadOp},
	}
	for _, c := range cases {
		if _, err := d.Patch(context.Background(), []docmodel.PatchOp{c.op}); !errors.Is(err, c.want) {
			t.Fatalf("%s: 错误 = %v，期望 %v", c.name, err, c.want)
		}
		if !bytes.Equal(d.Bytes(), src) {
			t.Fatalf("%s: 被拒绝的操作改动了文件", c.name)
		}
	}

	// 指纹正确时应当放行。
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op: docmodel.OpReplaceText, Node: "body/p1", Anchor: good, Text: "合同金额 200 元",
	}}); err != nil {
		t.Fatalf("anchor 正确却失败: %v", err)
	}
	if got := d.paras["body/p1"].text; got != "合同金额 200 元" {
		t.Fatalf("改后文本 = %q", got)
	}
}

// TestEmptyParagraphInsert 空段落（<w:p/> 与 <w:p></w:p>）也能写入第一段文本。
func TestEmptyParagraphInsert(t *testing.T) {
	for name, body := range map[string]string{
		"自闭合": `<w:p/>`,
		"空元素": `<w:p></w:p>`,
	} {
		t.Run(name, func(t *testing.T) {
			d := mustOpen(t, docxBody(t, body))
			if got := d.paras["body/p1"].text; got != "" {
				t.Fatalf("空段落投影 = %q", got)
			}
			if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
				Op: docmodel.OpReplaceText, Node: "body/p1", Text: "新补的一段",
			}}); err != nil {
				t.Fatalf("写入空段落失败: %v", err)
			}
			if got := d.paras["body/p1"].text; got != "新补的一段" {
				t.Fatalf("改后文本 = %q，期望 新补的一段", got)
			}
			if !strings.Contains(partOf(t, d.Bytes(), "word/document.xml"), "<w:r>") {
				t.Fatal("空段落没有生成 run")
			}
		})
	}
}

// TestSpaceAndNewlineRendering 首尾空格要加 xml:space，换行要变成 <w:br/>。
func TestSpaceAndNewlineRendering(t *testing.T) {
	d := mustOpen(t, docxBody(t, `<w:p><w:r><w:t>abc</w:t></w:r></w:p>`))
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op: docmodel.OpReplaceText, Node: "body/p1", Text: " 第一行\n第二行 ",
	}}); err != nil {
		t.Fatalf("patch 失败: %v", err)
	}
	if got, want := d.paras["body/p1"].text, " 第一行\n第二行 "; got != want {
		t.Fatalf("改后文本 = %q，期望 %q", got, want)
	}
	xml := partOf(t, d.Bytes(), "word/document.xml")
	if !strings.Contains(xml, "xml:space=\"preserve\"") {
		t.Fatalf("首尾空格没有 preserve：%s", xml)
	}
	if !strings.Contains(xml, "<w:br/>") {
		t.Fatalf("换行没有变成 <w:br/>：%s", xml)
	}
	if strings.Contains(xml, "\n第二行") {
		t.Fatalf("裸换行被直接写进了字符数据：%q", xml)
	}
}

// TestReadRangePagination 分页读：不指定节点读整篇，Next 走完为 0。
func TestReadRangePagination(t *testing.T) {
	d := mustOpen(t, docxBody(t, `<w:p><w:r><w:t>一二三四五</w:t></w:r></w:p>`+
		`<w:p><w:r><w:t>六七八</w:t></w:r></w:p>`))
	first, err := d.ReadRange(context.Background(), docmodel.Selector{Limit: 4})
	if err != nil {
		t.Fatalf("分页读失败: %v", err)
	}
	if first.Node != "body" || first.Total != len([]rune("一二三四五\n六七八")) {
		t.Fatalf("首块 = %+v", first)
	}
	if first.Text != "一二三四" || first.Next != 4 {
		t.Fatalf("首块文本 = %q, next = %d", first.Text, first.Next)
	}
	second, err := d.ReadRange(context.Background(), docmodel.Selector{Offset: first.Next, Limit: 100})
	if err != nil {
		t.Fatalf("第二块失败: %v", err)
	}
	if second.Text != "五\n六七八" || second.Next != 0 {
		t.Fatalf("第二块 = %+v", second)
	}
	// 指定节点只读那一段。
	one, err := d.ReadRange(context.Background(), docmodel.Selector{Node: "body/p2"})
	if err != nil {
		t.Fatalf("按节点读失败: %v", err)
	}
	if one.Text != "六七八" || one.Total != 3 || one.Next != 0 {
		t.Fatalf("body/p2 = %+v", one)
	}
	if _, err := d.ReadRange(context.Background(), docmodel.Selector{Node: "body/p9"}); !errors.Is(err, docmodel.ErrNodeNotFound) {
		t.Fatalf("未知节点应报 ErrNodeNotFound，得到 %v", err)
	}
}
