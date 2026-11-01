package docx

// 文本框 / 内容控件的回归：这些段落此前在 read 里可见，却没有节点地址，
// 导致 outline/read_range/find/patch/verify/digest 与 read 看到两份不同的文档。

import (
	"context"
	"testing"

	"github.com/NouSart/sump/files/docmodel"
)

// TestNestedParagraphsAreIndexed 文本框与内容控件里的段落必须有稳定地址，
// 且原有 body/pN、body/tblN 地址不变。
func TestNestedParagraphsAreIndexed(t *testing.T) {
	body := `<w:p><w:r><w:t>外层</w:t></w:r>` +
		`<w:r><w:drawing xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" ` +
		`xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" ` +
		`xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape">` +
		`<wp:inline><a:graphic><a:graphicData><wps:wsp><wps:txbx><w:txbxContent>` +
		`<w:p><w:r><w:t>框内</w:t></w:r></w:p>` +
		`</w:txbxContent></wps:txbx></wps:wsp></a:graphicData></a:graphic></wp:inline>` +
		`</w:drawing></w:r></w:p>` +
		`<w:sdt><w:sdtPr/><w:sdtContent>` +
		`<w:p><w:r><w:t>控件内</w:t></w:r></w:p>` +
		`</w:sdtContent></w:sdt>`

	d := mustOpen(t, docxBody(t, body))
	if got, want := d.Text(), "外层\n框内\n控件内"; got != want {
		t.Fatalf("整篇文本 = %q，期望 %q", got, want)
	}
	for _, id := range []docmodel.NodeID{"body/p1", "body/p1/p1", "body/sdt1/p1"} {
		if _, ok := d.paras[id]; !ok {
			t.Fatalf("缺少节点 %s；实际节点=%v", id, d.order)
		}
	}
	outline, err := d.Outline(context.Background(), 1)
	if err != nil {
		t.Fatalf("outline 失败: %v", err)
	}
	if len(outline.Nodes) != 3 {
		t.Fatalf("outline 顶层节点数 = %d，期望 3：%+v", len(outline.Nodes), outline.Nodes)
	}

	// 改文本框里的段落：外层段落一个字节都不能动。
	if _, err := d.Patch(context.Background(), []docmodel.PatchOp{{
		Op: docmodel.OpReplaceText, Node: "body/p1/p1", Text: "框内改",
	}}); err != nil {
		t.Fatalf("编辑文本框段落失败: %v", err)
	}
	if got := d.paras["body/p1"].text; got != "外层" {
		t.Fatalf("外层段落被误改: %q", got)
	}
	if got := d.paras["body/p1/p1"].text; got != "框内改" {
		t.Fatalf("文本框段落 = %q，期望 框内改", got)
	}
}

// TestInlineContentControlTextIncluded 行内内容控件没有嵌套段落，
// 但它包着的 run 仍然是本段文字；不能因为路径比 p→r→t 深就漏掉。
func TestInlineContentControlTextIncluded(t *testing.T) {
	body := `<w:p><w:r><w:t>前</w:t></w:r>` +
		`<w:sdt><w:sdtPr/><w:sdtContent><w:r><w:t>控件文字</w:t></w:r></w:sdtContent></w:sdt>` +
		`<w:ins><w:r><w:t>插入</w:t></w:r></w:ins>` +
		`<w:r><w:t>后</w:t></w:r></w:p>`
	d := mustOpen(t, docxBody(t, body))
	if got, want := d.Text(), "前控件文字插入后"; got != want {
		t.Fatalf("整篇文本 = %q，期望 %q", got, want)
	}
	outline, err := d.Outline(context.Background(), 1)
	if err != nil {
		t.Fatalf("outline 失败: %v", err)
	}
	if len(outline.Nodes) != 1 || outline.Nodes[0].Text != "前控件文字插入后" {
		t.Fatalf("段落预览不符: %+v", outline.Nodes)
	}
}

// TestAlternateContentIndexedOnce AlternateContent 的 Choice 是当前内容，
// Fallback 只是旧客户端兼容分支；两边都建节点会让同一段文字出现两次。
func TestAlternateContentIndexedOnce(t *testing.T) {
	body := `<w:p><w:r><w:t>正文</w:t></w:r>` +
		`<w:r><w:drawing><wp:inline xmlns:wp="http://schemas.openxmlformats.org/drawingml/2006/wordprocessingDrawing" ` +
		`xmlns:mc="http://schemas.openxmlformats.org/markup-compatibility/2006">` +
		`<mc:AlternateContent>` +
		`<mc:Choice Requires="wps"><w:txbxContent xmlns:wps="http://schemas.microsoft.com/office/word/2010/wordprocessingShape">` +
		`<w:p><w:r><w:t>新分支</w:t></w:r></w:p></w:txbxContent></mc:Choice>` +
		`<mc:Fallback><w:txbxContent><w:p><w:r><w:t>旧分支</w:t></w:r></w:p></w:txbxContent></mc:Fallback>` +
		`</mc:AlternateContent></wp:inline></w:drawing></w:r></w:p>`
	d := mustOpen(t, docxBody(t, body))
	if got, want := d.Text(), "正文\n新分支"; got != want {
		t.Fatalf("AlternateContent 文本 = %q，期望 %q", got, want)
	}
}
