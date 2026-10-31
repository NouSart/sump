package server

import (
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

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

// TestPdfToTextCorrupt 非 PDF 字节应明确报错，而不是 panic 或空返回。
func TestPdfToTextCorrupt(t *testing.T) {
	if _, _, err := pdfToText([]byte("不是 PDF"), 100, pdfMaxPages); err == nil {
		t.Fatal("损坏的 PDF 应报错")
	}
}

// TestEditPdfUnknownOp 未知 op 在触及 pdfcpu 之前即报错（无需合法 PDF）。
func TestEditPdfUnknownOp(t *testing.T) {
	_, _, err := editPdf([]byte("x"), []protocol.PdfEditOperation{{Op: "watermark"}})
	if err == nil || !strings.Contains(err.Error(), "不支持 op") {
		t.Fatalf("未知 op 应报错: %v", err)
	}
}
