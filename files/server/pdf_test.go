package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/pdfcpu/pdfcpu/pkg/api"
	"github.com/pdfcpu/pdfcpu/pkg/pdfcpu/model"

	"github.com/NouSart/sump/protocol"
)

// testPdf 构造一个最小合法 PDF：每页一句文本，xref 偏移按实际字节算。
func testPdf(t *testing.T, texts ...string) []byte {
	t.Helper()
	n := len(texts)
	fontNum := 3 + 2*n
	kids := make([]string, n)
	for i := range kids {
		kids[i] = fmt.Sprintf("%d 0 R", 3+i)
	}
	var body bytes.Buffer
	body.WriteString("%PDF-1.4\n")
	type obj struct{ off int }
	var objs []obj
	emit := func(num int, content string) {
		objs = append(objs, obj{body.Len()})
		fmt.Fprintf(&body, "%d 0 obj\n%s\nendobj\n", num, content)
	}
	emit(1, "<< /Type /Catalog /Pages 2 0 R >>")
	emit(2, fmt.Sprintf("<< /Type /Pages /Kids [%s] /Count %d >>", strings.Join(kids, " "), n))
	for i := 0; i < n; i++ {
		emit(3+i, fmt.Sprintf("<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents %d 0 R /Resources << /Font << /F1 %d 0 R >> >> >>", 3+n+i, fontNum))
	}
	for i, s := range texts {
		stream := fmt.Sprintf("BT /F1 24 Tf 72 720 Td (%s) Tj ET", s)
		emit(3+n+i, fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(stream), stream))
	}
	emit(fontNum, "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>")

	xref := body.Len()
	fmt.Fprintf(&body, "xref\n0 %d\n", len(objs)+1)
	body.WriteString("0000000000 65535 f\r\n")
	for _, o := range objs {
		fmt.Fprintf(&body, "%010d 00000 n\r\n", o.off)
	}
	fmt.Fprintf(&body, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objs)+1, xref)
	return body.Bytes()
}

// encryptedPdf 用 pdfcpu 给 testPdf 加用户密码，得到一份真的加密文件。
func encryptedPdf(t *testing.T, src []byte) []byte {
	t.Helper()
	conf := model.NewDefaultConfiguration()
	conf.UserPW = "s3cret"
	conf.OwnerPW = "s3cret"
	var out bytes.Buffer
	if err := api.Encrypt(context.Background(), bytes.NewReader(src), &out, conf); err != nil {
		t.Fatalf("构造加密 PDF 失败: %v", err)
	}
	return out.Bytes()
}

// TestParsePageSelection 覆盖页选集解析的去重、升序、区间与各类非法输入。
func TestParsePageSelection(t *testing.T) {
	cases := []struct {
		name    string
		sel     string
		total   int
		want    string
		wantErr string
	}{
		{"单页", "2", 3, "2", ""},
		{"区间", "1-3", 3, "1,2,3", ""},
		{"混合去重升序", "1,3-4,2", 4, "1,2,3,4", ""},
		{"越界", "5", 3, "", "无效"},
		{"降序区间", "3-1", 3, "", "无效"},
		{"非数字", "a", 3, "", "无效"},
		{"空段", "1,,2", 3, "1,2", ""},
		{"全空", ", ,", 3, "", "无效"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := parsePageSelection(c.sel, c.total)
			if c.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), c.wantErr) {
					t.Fatalf("应报错含 %q，得 %v", c.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if strings.Join(got, ",") != c.want {
				t.Fatalf("got %v, want %v", got, c.want)
			}
		})
	}
}

// TestPdfToTextPageLimit 页数上限真的会截断：两页的 PDF 只读第一页，并标记 truncated。
// （这条防线由 pdfToText 的 pageLimit 参数承载，不需要改全局变量。）
func TestPdfToTextPageLimit(t *testing.T) {
	data := testPdf(t, "Hello", "World")
	text, truncated, err := pdfToText(data, 1000, 1)
	if err != nil {
		t.Fatal(err)
	}
	if !truncated {
		t.Fatal("超出页数上限应标记 truncated")
	}
	if !strings.Contains(text, "# 第 1 页") || strings.Contains(text, "# 第 2 页") {
		t.Fatalf("只应读到第 1 页，实际:\n%s", text)
	}
}

// TestPdfToTextCorrupt 非 PDF 字节应明确报错，而不是 panic 或空返回。
func TestPdfToTextCorrupt(t *testing.T) {
	if _, _, err := pdfToText([]byte("不是 PDF"), 100, pdfMaxPages); err == nil {
		t.Fatal("损坏的 PDF 应报错")
	}
}

// TestPdfToTextEncrypted 加密 PDF 报的是「已加密」，不是含糊的「可能已加密或已损坏」。
func TestPdfToTextEncrypted(t *testing.T) {
	data := encryptedPdf(t, testPdf(t, "Hello"))
	_, _, err := pdfToText(data, 1000, pdfMaxPages)
	if err == nil {
		t.Fatal("加密 PDF 应报错")
	}
	if !errors.Is(err, errPdfEncrypted) {
		t.Fatalf("应是加密错误，实际: %v", err)
	}
}

// TestEditPdfEncrypted 编辑加密 PDF 时抛的是同一条中文理由，而不是 pdfcpu 的英文库错误。
func TestEditPdfEncrypted(t *testing.T) {
	data := encryptedPdf(t, testPdf(t, "Hello"))
	_, _, err := editPdf(data, []protocol.PdfEditOperation{{Op: "rotate", Degrees: 90}})
	if err == nil {
		t.Fatal("加密 PDF 应报错")
	}
	if !errors.Is(err, errPdfEncrypted) {
		t.Fatalf("应是加密错误，实际: %v", err)
	}
	if strings.Contains(err.Error(), "password") {
		t.Fatalf("不应把 pdfcpu 的英文原文透出去: %v", err)
	}
}

// TestEditPdfUnknownOp 未知 op 在触及 pdfcpu 之前即报错（无需合法 PDF）。
func TestEditPdfUnknownOp(t *testing.T) {
	_, _, err := editPdf([]byte("x"), []protocol.PdfEditOperation{{Op: "watermark"}})
	if err == nil || !strings.Contains(err.Error(), "不支持 op") {
		t.Fatalf("未知 op 应报错: %v", err)
	}
}
