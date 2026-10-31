package tests

// files PDF 测试：.pdf 读取（按页分节 / 截断 / 扫描件拒绝）与 edit_pdf（元数据 / 页操作 / 错误路径）。

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"strings"
	"testing"

	"github.com/ledongthuc/pdf"
	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/NouSart/sump/protocol"
)

// makePdf 在内存中构造一个最小合法 PDF，每页一个文本内容流（由调用方给定，如 "BT /F1 24 Tf 72 720 Td (Hello) Tj ET"）。
// xref 偏移按实际写入字节计算，保证可被 pdfcpu / ledongthuc 解析。
func makePdf(t *testing.T, pages ...string) []byte {
	t.Helper()
	n := len(pages)
	fontNum := 3 + 2*n
	kids := make([]string, n)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", 3+i)
	}

	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	type obj struct{ num, off int }
	var objs []obj
	emit := func(num int, content string) {
		objs = append(objs, obj{num, body.Len()})
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", num, content)
	}

	emit(1, "<< /Type /Catalog /Pages 2 0 R >>")
	emit(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for i := 0; i < n; i++ {
		emit(3+i, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", 3+n+i, fontNum))
	}
	for i, s := range pages {
		emit(3+n+i, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(s), s))
	}
	emit(fontNum, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xref := body.Len()
	fmt.Fprintf(&body, "xref\n0 %d\n", len(objs)+1)
	fmt.Fprintf(&body, "0000000000 65535 f\r\n")
	for _, o := range objs {
		fmt.Fprintf(&body, "%010d 00000 n\r\n", o.off)
	}
	fmt.Fprintf(&body, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)

	return body.Bytes()
}

// savePdf 存入一段 PDF 并返回 id。
func savePdf(t *testing.T, addr, filename string, data []byte) string {
	t.Helper()
	rp := call(t, addr, "save", protocol.FileSavePayload{
		Data:     base64.StdEncoding.EncodeToString(data),
		Filename: filename,
	})
	if !rp.OK {
		t.Fatalf("save pdf 失败: %s", rp.Error)
	}
	var sr protocol.FileSaveResult
	if err := protocol.DecodeRaw(rp.Data, &sr); err != nil {
		t.Fatal(err)
	}
	return sr.ID
}

// fetchPdf 取回文件二进制。
func fetchPdf(t *testing.T, addr, id string) []byte {
	t.Helper()
	rp := call(t, addr, "fetch", protocol.FileFetchPayload{ID: id})
	if !rp.OK {
		t.Fatalf("fetch 失败: %s", rp.Error)
	}
	var fr protocol.FileFetchResult
	if err := protocol.DecodeRaw(rp.Data, &fr); err != nil {
		t.Fatal(err)
	}
	data, err := base64.StdEncoding.DecodeString(fr.Data)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// pdfPageCount 用 pdfcpu 读回页数（验证页操作结果）。
func pdfPageCount(t *testing.T, addr, id string) int {
	t.Helper()
	conf := model.NewDefaultConfiguration()
	n, err := api.PageCount(context.Background(), bytes.NewReader(fetchPdf(t, addr, id)), conf)
	if err != nil {
		t.Fatalf("PageCount 失败: %v", err)
	}
	return n
}

// pdfProperty 用 ledongthuc 读回 Info 字典里的单个键（验证 set_info 结果）。
// 不用 pdfcpu 的 api.Properties：该函数返回的 ctx.Properties 在本版本不填充 Info 字典。
func pdfProperty(t *testing.T, addr, id, key string) string {
	t.Helper()
	data := fetchPdf(t, addr, id)
	r, err := pdf.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatalf("解析 pdf 失败: %v", err)
	}
	return r.Trailer().Key("Info").Key(key).Text()
}

// TestReadPdf 读取 .pdf：按页输出，顺序正确，未截断。
func TestReadPdf(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "报告.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (Hello) Tj ET",
		"BT /F1 24 Tf 72 720 Td (World) Tj ET",
	))

	rr := readFile(t, srv.WsURL(), id)
	if rr.Truncated {
		t.Fatalf("小文件不应截断: %+v", rr)
	}
	want := "# 第 1 页\nHello\n# 第 2 页\nWorld"
	if rr.Text != want {
		t.Fatalf("读取结果不符:\n%q\n想要:\n%q", rr.Text, want)
	}
}

// TestReadPdfTruncated 小阈值下在页内按字符截断。
func TestReadPdfTruncated(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 10) // 仅够 "# 第 1 页\n" + "He"
	id := savePdf(t, srv.WsURL(), "报告.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (Hello) Tj ET",
		"BT /F1 24 Tf 72 720 Td (World) Tj ET",
	))

	rr := readFile(t, srv.WsURL(), id)
	if !rr.Truncated {
		t.Fatalf("应截断: %+v", rr)
	}
	if rr.Text != "# 第 1 页\nHe" {
		t.Fatalf("截断结果不符: %q", rr.Text)
	}
}

// TestReadPdfScanned 无文本层（图片型）的 PDF read 应报扫描件错误。
func TestReadPdfScanned(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "扫描.pdf", makePdf(t, "0 0 100 100 re f"))

	rp := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: id})
	if rp.OK || !strings.Contains(rp.Error, "扫描件") {
		t.Fatalf("无文本层应报扫描件错误: %+v", rp)
	}
}

// TestEditPdf 元数据与旋转生效：Applied 计数、页数不变、元数据写入。
func TestEditPdf(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "报告.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (Hello) Tj ET",
		"BT /F1 24 Tf 72 720 Td (World) Tj ET",
	))

	rp := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id,
		Operations: []protocol.PdfEditOperation{
			{Op: "set_info", Properties: map[string]string{"Title": "新标题", "Author": "测试"}},
			{Op: "rotate", Pages: "1", Degrees: 90},
		},
	})
	if !rp.OK {
		t.Fatalf("edit_pdf 失败: %s", rp.Error)
	}
	var pr protocol.FilePdfEditResult
	if err := protocol.DecodeRaw(rp.Data, &pr); err != nil {
		t.Fatal(err)
	}
	if pr.Applied != 2 || pr.ID != id {
		t.Fatalf("edit_pdf 结果不符: %+v", pr)
	}

	if n := pdfPageCount(t, srv.WsURL(), id); n != 2 {
		t.Fatalf("rotate 后页数应为 2，得 %d", n)
	}
	if got := pdfProperty(t, srv.WsURL(), id, "Title"); got != "新标题" {
		t.Fatalf("set_info 未生效: Title=%q", got)
	}
}

// TestEditPdfPages 删页 / 保留页改变页数。
func TestEditPdfPages(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "三页.pdf", makePdf(t,
		"BT /F1 24 Tf 72 720 Td (One) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Two) Tj ET",
		"BT /F1 24 Tf 72 720 Td (Three) Tj ET",
	))
	if n := pdfPageCount(t, srv.WsURL(), id); n != 3 {
		t.Fatalf("初始页数应为 3，得 %d", n)
	}

	rp := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "delete_pages", Pages: "2"}},
	})
	if !rp.OK {
		t.Fatalf("delete_pages 失败: %s", rp.Error)
	}
	if n := pdfPageCount(t, srv.WsURL(), id); n != 2 {
		t.Fatalf("删页后应为 2 页，得 %d", n)
	}

	rp2 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "keep_pages", Pages: "1"}},
	})
	if !rp2.OK {
		t.Fatalf("keep_pages 失败: %s", rp2.Error)
	}
	if n := pdfPageCount(t, srv.WsURL(), id); n != 1 {
		t.Fatalf("保留页后应为 1 页，得 %d", n)
	}
}

// TestEditPdfErrors 错误路径：非 pdf、空 operations、未知 op、越界页、非法角度、缺 pages、缺 properties。
func TestEditPdfErrors(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	id := savePdf(t, srv.WsURL(), "报告.pdf", makePdf(t, "BT /F1 24 Tf 72 720 Td (Hello) Tj ET"))

	// 非 pdf。
	txtID := saveText(t, srv.WsURL(), "a.txt", "文本")
	bad1 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: txtID, Operations: []protocol.PdfEditOperation{{Op: "rotate", Degrees: 90}},
	})
	if bad1.OK || !strings.Contains(bad1.Error, "仅支持编辑 .pdf") {
		t.Fatalf("非 pdf 应拒绝: %+v", bad1)
	}

	// 空 operations。
	bad2 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{ID: id})
	if bad2.OK || !strings.Contains(bad2.Error, "operations 至少一条") {
		t.Fatalf("空 operations 应报错: %+v", bad2)
	}

	// 未知 op。
	bad3 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "watermark"}},
	})
	if bad3.OK || !strings.Contains(bad3.Error, "不支持 op") {
		t.Fatalf("未知 op 应报错: %+v", bad3)
	}

	// 越界页选集。
	bad4 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "delete_pages", Pages: "9"}},
	})
	if bad4.OK || !strings.Contains(bad4.Error, "无效") {
		t.Fatalf("越界页选集应报错: %+v", bad4)
	}

	// 非法角度。
	bad5 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "rotate", Degrees: 45}},
	})
	if bad5.OK || !strings.Contains(bad5.Error, "90 的整数倍") {
		t.Fatalf("非法角度应报错: %+v", bad5)
	}

	// delete_pages 缺 pages。
	bad6 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "delete_pages"}},
	})
	if bad6.OK || !strings.Contains(bad6.Error, "需要 pages") {
		t.Fatalf("delete_pages 缺 pages 应报错: %+v", bad6)
	}

	// set_info 缺 properties。
	bad7 := call(t, srv.WsURL(), "edit_pdf", protocol.FilePdfEditPayload{
		ID: id, Operations: []protocol.PdfEditOperation{{Op: "set_info"}},
	})
	if bad7.OK || !strings.Contains(bad7.Error, "需要 properties") {
		t.Fatalf("set_info 缺 properties 应报错: %+v", bad7)
	}
}
