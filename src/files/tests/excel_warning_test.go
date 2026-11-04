package tests

// 损坏工作表的回归：单表读取失败不能让 read 静默返回“完整”结果。

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"

	"github.com/NouSart/sump/protocol"
)

// replaceZipEntry 重建 zip，只替换指定条目（保留其它条目的原始字节）。
func replaceZipEntry(t *testing.T, data []byte, name string, replacement []byte) []byte {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, f := range zr.File {
		h := &zip.FileHeader{Name: f.Name, Method: f.Method, Modified: f.Modified}
		w, err := zw.CreateHeader(h)
		if err != nil {
			t.Fatal(err)
		}
		if f.Name == name {
			if _, err := w.Write(replacement); err != nil {
				t.Fatal(err)
			}
			continue
		}
		rc, err := f.Open()
		if err != nil {
			t.Fatal(err)
		}
		_, err = io.Copy(w, rc)
		rc.Close()
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// TestXlsxBrokenSheetIsNotSilent 单张表损坏时，要么 read 明确失败，
// 要么成功结果必须带 warning 且 truncated=true——不能少一张表还说完整。
func TestXlsxBrokenSheetIsNotSilent(t *testing.T) {
	sc := startStubCenter(t)
	srv := startFiles(t, sc.URL(), 4000)
	broken := replaceZipEntry(t, makeXlsx(t), "xl/worksheets/sheet2.xml",
		[]byte(`<worksheet><sheetData><row><c r="A1"><v>1</v></c>`))
	id := saveXlsx(t, srv.WsURL(), "坏了一张表.xlsx", broken)

	rp := call(t, srv.WsURL(), "read", protocol.FileReadPayload{ID: id})
	if !rp.OK {
		// 打开阶段就拒绝也算“不静默”；错误必须指出文件/解析问题。
		if !strings.Contains(rp.Error, "xlsx") && !strings.Contains(rp.Error, "读取") {
			t.Fatalf("坏表被拒但错误不明确: %s", rp.Error)
		}
		return
	}
	var rr protocol.FileReadResult
	if err := protocol.DecodeRaw(rp.Data, &rr); err != nil {
		t.Fatal(err)
	}
	if !rr.Truncated || len(rr.Warnings) == 0 {
		t.Fatalf("坏表被静默跳过: %+v", rr)
	}
	joined := strings.Join(rr.Warnings, "\n")
	if !strings.Contains(joined, "失败") || !strings.Contains(joined, "结果不完整") {
		t.Fatalf("warning 没说清哪张表失败: %v", rr.Warnings)
	}
}
