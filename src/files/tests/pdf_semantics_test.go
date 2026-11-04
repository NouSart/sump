package tests

// PDF 语义实验：页数正确不代表删对页、留对页、转对页。
// 验证器用独立的 ledongthuc 读取每页文本与从页面字典继承的 /Rotate，
// 不依赖执行编辑的 pdfcpu 输出。

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"

	"github.com/NouSart/sump/protocol"
)

// pdfSemantic 取回并解析 PDF，返回逐页文本与旋转角。
type pdfSemantic struct {
	texts    []string
	rotates  []int
	pageNums int
}

func inspectPdf(t *testing.T, addr, id string) pdfSemantic {
	t.Helper()
	data := fetchPdf(t, addr, id)
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("解析编辑后的 PDF 失败: %v", err)
	}
	n := r.NumPage()
	out := pdfSemantic{pageNums: n, texts: make([]string, n), rotates: make([]int, n)}
	for i := 1; i <= n; i++ {
		text, err := r.Page(i).GetPlainText(nil)
		if err != nil {
			t.Fatalf("提取第 %d 页文本失败: %v", i, err)
		}
		out.texts[i-1] = strings.TrimSpace(text)
		out.rotates[i-1] = pdfPageRotation(t, r, i)
	}
	return out
}

// pdfPageRotation 沿 Parent 链读取继承的 /Rotate（页面自身可能不直接带该键）。
func pdfPageRotation(t *testing.T, r *pdf.Reader, pageNum int) int {
	t.Helper()
	v := r.Page(pageNum).V
	for depth := 0; depth < 32 && !v.IsNull(); depth++ {
		if rot := v.Key("Rotate"); !rot.IsNull() {
			return int(rot.Int64())
		}
		v = v.Key("Parent")
	}
	if !v.IsNull() {
		t.Fatalf("第 %d 页 Parent 链超过 32 层，拒绝把测试挂死", pageNum)
	}
	return 0
}

func assertPdf(t *testing.T, got pdfSemantic, wantTexts []string, wantRotates []int) {
	t.Helper()
	if got.pageNums != len(wantTexts) || got.pageNums != len(wantRotates) {
		t.Fatalf("页数=%d，期望文本 %d 页、旋转 %d 页", got.pageNums, len(wantTexts), len(wantRotates))
	}
	for i := range wantTexts {
		if got.texts[i] != wantTexts[i] {
			t.Fatalf("第 %d 页内容=%q，期望 %q（全部=%q）", i+1, got.texts[i], wantTexts[i], got.texts)
		}
		if got.rotates[i] != wantRotates[i] {
			t.Fatalf("第 %d 页旋转=%d，期望 %d（全部=%v）", i+1, got.rotates[i], wantRotates[i], got.rotates)
		}
	}
}

// TestEditPdfRotateOnlySelectedPage 验证转的是指定页，非目标页保持原方向。
func TestEditPdfRotateOnlySelectedPage(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "旋转.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (One) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Two) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Three) Tj ET",
	))

	rp := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{ID: id,
		Operations: []protocol.PdfEditOperation{{Op: "rotate", Pages: "2", Degrees: 90}}})
	if !rp.OK {
		t.Fatalf("rotate 失败: %s", rp.Error)
	}
	assertPdf(t, inspectPdf(t, srv.WsURL(), id),
		[]string{"One", "Two", "Three"},
		[]int{0, 90, 0})
}

// TestEditPdfDeletePreservesPageContentAndOrder 验证删的是第 2 页，剩下的内容与顺序不变。
func TestEditPdfDeletePreservesPageContentAndOrder(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "删页.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (One) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Two) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Three) Tj ET",
	))

	rp := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{ID: id,
		Operations: []protocol.PdfEditOperation{{Op: "delete_pages", Pages: "2"}}})
	if !rp.OK {
		t.Fatalf("delete_pages 失败: %s", rp.Error)
	}
	assertPdf(t, inspectPdf(t, srv.WsURL(), id),
		[]string{"One", "Three"},
		[]int{0, 0})
}

// TestEditPdfKeepPreservesSelectedContent 验证只保留第 1、3 页，内容与顺序正确。
func TestEditPdfKeepPreservesSelectedContent(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "保留页.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (One) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Two) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Three) Tj ET",
	))

	rp := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{ID: id,
		Operations: []protocol.PdfEditOperation{{Op: "keep_pages", Pages: "1,3"}}})
	if !rp.OK {
		t.Fatalf("keep_pages 失败: %s", rp.Error)
	}
	assertPdf(t, inspectPdf(t, srv.WsURL(), id),
		[]string{"One", "Three"},
		[]int{0, 0})
}

// TestEditPdfCombinedOpsUseCurrentPageNumbers 先删第 2 页，再旋转"当前"第 2 页（原第 3 页）。
// 若实现始终按原始页号解析，会旋转已被删除的页或漏掉 Three。
func TestEditPdfCombinedOpsUseCurrentPageNumbers(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "组合.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (One) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Two) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Three) Tj ET",
	))

	rp := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{ID: id,
		Operations: []protocol.PdfEditOperation{
			{Op: "delete_pages", Pages: "2"},
			{Op: "rotate", Pages: "2", Degrees: 90},
		}})
	if !rp.OK {
		t.Fatalf("组合编辑失败: %s", rp.Error)
	}
	assertPdf(t, inspectPdf(t, srv.WsURL(), id),
		[]string{"One", "Three"},
		[]int{0, 90})
}
